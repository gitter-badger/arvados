#!/usr/bin/env python

# TODO:
# --md5sum - display md5 of each file as read from disk

import argparse
import arvados
import base64
import errno
import fcntl
import hashlib
import json
import os
import signal
import sys
import tempfile

CAUGHT_SIGNALS = [signal.SIGINT, signal.SIGQUIT, signal.SIGTERM]

def parse_arguments(arguments):
    parser = argparse.ArgumentParser(
        description='Copy data from the local filesystem to Keep.')

    parser.add_argument('paths', metavar='path', type=str, nargs='*',
                        help="""
    Local file or directory. Default: read from standard input.
    """)

    parser.add_argument('--max-manifest-depth', type=int, metavar='N',
                        default=-1, help="""
    Maximum depth of directory tree to represent in the manifest
    structure. A directory structure deeper than this will be represented
    as a single stream in the manifest. If N=0, the manifest will contain
    a single stream. Default: -1 (unlimited), i.e., exactly one manifest
    stream per filesystem directory that contains files.
    """)

    group = parser.add_mutually_exclusive_group()

    group.add_argument('--as-stream', action='store_true', dest='stream',
                       help="""
    Synonym for --stream.
    """)

    group.add_argument('--stream', action='store_true',
                       help="""
    Store the file content and display the resulting manifest on
    stdout. Do not write the manifest to Keep or save a Collection object
    in Arvados.
    """)

    group.add_argument('--as-manifest', action='store_true', dest='manifest',
                       help="""
    Synonym for --manifest.
    """)

    group.add_argument('--in-manifest', action='store_true', dest='manifest',
                       help="""
    Synonym for --manifest.
    """)

    group.add_argument('--manifest', action='store_true',
                       help="""
    Store the file data and resulting manifest in Keep, save a Collection
    object in Arvados, and display the manifest locator (Collection uuid)
    on stdout. This is the default behavior.
    """)

    group.add_argument('--as-raw', action='store_true', dest='raw',
                       help="""
    Synonym for --raw.
    """)

    group.add_argument('--raw', action='store_true',
                       help="""
    Store the file content and display the data block locators on stdout,
    separated by commas, with a trailing newline. Do not store a
    manifest.
    """)

    parser.add_argument('--use-filename', type=str, default=None,
                        dest='filename', help="""
    Synonym for --filename.
    """)

    parser.add_argument('--filename', type=str, default=None,
                        help="""
    Use the given filename in the manifest, instead of the name of the
    local file. This is useful when "-" or "/dev/stdin" is given as an
    input file. It can be used only if there is exactly one path given and
    it is not a directory. Implies --manifest.
    """)

    group = parser.add_mutually_exclusive_group()
    group.add_argument('--progress', action='store_true',
                       help="""
    Display human-readable progress on stderr (bytes and, if possible,
    percentage of total data size). This is the default behavior when
    stderr is a tty.
    """)

    group.add_argument('--no-progress', action='store_true',
                       help="""
    Do not display human-readable progress on stderr, even if stderr is a
    tty.
    """)

    group.add_argument('--batch-progress', action='store_true',
                       help="""
    Display machine-readable progress on stderr (bytes and, if known,
    total data size).
    """)

    group = parser.add_mutually_exclusive_group()
    group.add_argument('--resume', action='store_true', default=True,
                       help="""
    Continue interrupted uploads from cached state (default).
    """)
    group.add_argument('--no-resume', action='store_false', dest='resume',
                       help="""
    Do not continue interrupted uploads from cached state.
    """)

    args = parser.parse_args(arguments)

    if len(args.paths) == 0:
        args.paths += ['/dev/stdin']

    if len(args.paths) != 1 or os.path.isdir(args.paths[0]):
        if args.filename:
            parser.error("""
    --filename argument cannot be used when storing a directory or
    multiple files.
    """)

    # Turn on --progress by default if stderr is a tty.
    if (not (args.batch_progress or args.no_progress)
        and os.isatty(sys.stderr.fileno())):
        args.progress = True

    if args.paths == ['-']:
        args.paths = ['/dev/stdin']
        if not args.filename:
            args.filename = '-'

    return args

class ResumeCacheConflict(Exception):
    pass


class ResumeCache(object):
    CACHE_DIR = os.path.expanduser('~/.cache/arvados/arv-put')

    @classmethod
    def setup_user_cache(cls):
        try:
            os.makedirs(cls.CACHE_DIR)
        except OSError as error:
            if error.errno != errno.EEXIST:
                raise
        else:
            os.chmod(cls.CACHE_DIR, 0o700)

    def __init__(self, file_spec):
        self.cache_file = open(file_spec, 'a+')
        self._lock_file(self.cache_file)
        self.filename = self.cache_file.name

    @classmethod
    def make_path(cls, args):
        md5 = hashlib.md5()
        md5.update(arvados.config.get('ARVADOS_API_HOST', '!nohost'))
        realpaths = sorted(os.path.realpath(path) for path in args.paths)
        md5.update(''.join(realpaths))
        if any(os.path.isdir(path) for path in realpaths):
            md5.update(str(max(args.max_manifest_depth, -1)))
        elif args.filename:
            md5.update(args.filename)
        return os.path.join(cls.CACHE_DIR, md5.hexdigest())

    def _lock_file(self, fileobj):
        try:
            fcntl.flock(fileobj, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except IOError:
            raise ResumeCacheConflict("{} locked".format(fileobj.name))

    def load(self):
        self.cache_file.seek(0)
        return json.load(self.cache_file)

    def save(self, data):
        try:
            new_cache_fd, new_cache_name = tempfile.mkstemp(
                dir=os.path.dirname(self.filename))
            self._lock_file(new_cache_fd)
            new_cache = os.fdopen(new_cache_fd, 'r+')
            json.dump(data, new_cache)
            os.rename(new_cache_name, self.filename)
        except (IOError, OSError, ResumeCacheConflict) as error:
            try:
                os.unlink(new_cache_name)
            except NameError:  # mkstemp failed.
                pass
        else:
            self.cache_file.close()
            self.cache_file = new_cache

    def close(self):
        self.cache_file.close()

    def destroy(self):
        try:
            os.unlink(self.filename)
        except OSError as error:
            if error.errno != errno.ENOENT:  # That's what we wanted anyway.
                raise
        self.close()

    def restart(self):
        self.destroy()
        self.__init__(self.filename)


class ArvPutCollectionWriter(arvados.ResumableCollectionWriter):
    STATE_PROPS = (arvados.ResumableCollectionWriter.STATE_PROPS +
                   ['bytes_written', '_seen_inputs'])

    def __init__(self, cache=None, reporter=None, bytes_expected=None):
        self.bytes_written = 0
        self._seen_inputs = []
        self.cache = cache
        self.reporter = reporter
        self.bytes_expected = bytes_expected
        super(ArvPutCollectionWriter, self).__init__()

    @classmethod
    def from_cache(cls, cache, reporter=None, bytes_expected=None):
        try:
            state = cache.load()
            state['_data_buffer'] = [base64.decodestring(state['_data_buffer'])]
            writer = cls.from_state(state, cache, reporter, bytes_expected)
        except (TypeError, ValueError,
                arvados.errors.StaleWriterStateError) as error:
            return cls(cache, reporter, bytes_expected)
        else:
            return writer

    def cache_state(self):
        state = self.dump_state()
        # Transform attributes for serialization.
        for attr, value in state.items():
            if attr == '_data_buffer':
                state[attr] = base64.encodestring(''.join(value))
            elif hasattr(value, 'popleft'):
                state[attr] = list(value)
        self.cache.save(state)

    def report_progress(self):
        if self.reporter is not None:
            self.reporter(self.bytes_written, self.bytes_expected)

    def flush_data(self):
        bytes_buffered = self._data_buffer_len
        super(ArvPutCollectionWriter, self).flush_data()
        # Checkpoint and report progress if data was PUT to Keep.
        if self._data_buffer_len < start_buffer_len:
            self.bytes_written += (start_buffer_len - self._data_buffer_len)
            self.report_progress()

    def _record_new_input(self, input_type, source_name, dest_name):
        # The key needs to be a list because that's what we'll get back
        # from JSON deserialization.
        key = [input_type, source_name, dest_name]
        if key in self._seen_inputs:
            return False
        self._seen_inputs.append(key)
        return True

    def write_file(self, source, filename=None):
        if self._record_new_input('file', source, filename):
            super(ArvPutCollectionWriter, self).write_file(source, filename)

    def write_directory_tree(self,
                             path, stream_name='.', max_manifest_depth=-1):
        if self._record_new_input('directory', path, stream_name):
            super(ArvPutCollectionWriter, self).write_directory_tree(
                path, stream_name, max_manifest_depth)


def expected_bytes_for(pathlist):
    # Walk the given directory trees and stat files, adding up file sizes,
    # so we can display progress as percent
    bytesum = 0
    for path in pathlist:
        if os.path.isdir(path):
            for filename in arvados.util.listdir_recursive(path):
                bytesum += os.path.getsize(os.path.join(path, filename))
        elif not os.path.isfile(path):
            return None
        else:
            bytesum += os.path.getsize(path)
    return bytesum

_machine_format = "{} {}: {{}} written {{}} total\n".format(sys.argv[0],
                                                            os.getpid())
def machine_progress(bytes_written, bytes_expected):
    return _machine_format.format(
        bytes_written, -1 if (bytes_expected is None) else bytes_expected)

def human_progress(bytes_written, bytes_expected):
    if bytes_expected:
        return "\r{}M / {}M {:.1%} ".format(
            bytes_written >> 20, bytes_expected >> 20,
            float(bytes_written) / bytes_expected)
    else:
        return "\r{} ".format(bytes_written)

def progress_writer(progress_func, outfile=sys.stderr):
    def write_progress(bytes_written, bytes_expected):
        outfile.write(progress_func(bytes_written, bytes_expected))
    return write_progress

def main(arguments=None):
    ResumeCache.setup_user_cache()
    args = parse_arguments(arguments)

    if args.progress:
        reporter = progress_writer(human_progress)
    elif args.batch_progress:
        reporter = progress_writer(machine_progress)
    else:
        reporter = None

    try:
        resume_cache = ResumeCache(ResumeCache.make_path(args))
        if not args.resume:
            resume_cache.restart()
    except ResumeCacheConflict:
        print "arv-put: Another process is already uploading this data."
        sys.exit(1)

    writer = ArvPutCollectionWriter.from_cache(
        resume_cache, reporter, expected_bytes_for(args.paths))

    def signal_handler(sigcode, frame):
        writer.cache_state()
        sys.exit(-sigcode)
    # Install our signal handler for each code in CAUGHT_SIGNALS, and save
    # the originals.
    orig_signal_handlers = {sigcode: signal.signal(sigcode, signal_handler)
                            for sigcode in CAUGHT_SIGNALS}

    if writer.bytes_written > 0:  # We're resuming a previous upload.
        print >>sys.stderr, "arv-put: Resuming previous upload.  Bypass with the --no-resume option."
        writer.report_progress()

    try:
        writer.do_queued_work()  # Do work resumed from cache.
        for path in args.paths:  # Copy file data to Keep.
            if os.path.isdir(path):
                writer.write_directory_tree(
                    path, max_manifest_depth=args.max_manifest_depth)
            else:
                writer.start_new_stream()
                writer.write_file(path, args.filename or os.path.basename(path))
    except Exception:
        writer.cache_state()
        raise

    if args.stream:
        print writer.manifest_text(),
    elif args.raw:
        writer.finish_current_stream()
        print ','.join(writer.data_locators())
    else:
        # Register the resulting collection in Arvados.
        arvados.api().collections().create(
            body={
                'uuid': writer.finish(),
                'manifest_text': writer.manifest_text(),
                },
            ).execute()

        # Print the locator (uuid) of the new collection.
        print writer.finish()

    for sigcode, orig_handler in orig_signal_handlers.items():
        signal.signal(sigcode, orig_handler)

    resume_cache.destroy()

if __name__ == '__main__':
    main()
