/* Provides low-level Get/Put primitives for accessing Arvados Keep blocks. */
package keepclient

import (
	"bytes"
	"crypto/md5"
	"crypto/tls"
	"errors"
	"fmt"
	"git.curoverse.com/arvados.git/sdk/go/arvadosclient"
	"git.curoverse.com/arvados.git/sdk/go/streamer"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// A Keep "block" is 64MB.
const BLOCKSIZE = 64 * 1024 * 1024

var BlockNotFound = errors.New("Block not found")
var InsufficientReplicasError = errors.New("Could not write sufficient replicas")
var OversizeBlockError = errors.New("Exceeded maximum block size (" + strconv.Itoa(BLOCKSIZE) + ")")
var MissingArvadosApiHost = errors.New("Missing required environment variable ARVADOS_API_HOST")
var MissingArvadosApiToken = errors.New("Missing required environment variable ARVADOS_API_TOKEN")
var InvalidLocatorError = errors.New("Invalid locator")
var KeepServerError = errors.New("One or more keep servers returned an error")

// ErrNoSuchKeepServer is returned when GetIndex is invoked with a UUID with no matching keep server
var ErrNoSuchKeepServer = errors.New("No keep server matching the given UUID is found")

// ErrIncompleteIndex is returned when the Index response does not end with a new empty line
var ErrIncompleteIndex = errors.New("Got incomplete index")

const X_Keep_Desired_Replicas = "X-Keep-Desired-Replicas"
const X_Keep_Replicas_Stored = "X-Keep-Replicas-Stored"

// Information about Arvados and Keep servers.
type KeepClient struct {
	Arvados            *arvadosclient.ArvadosClient
	Want_replicas      int
	Using_proxy        bool
	localRoots         *map[string]string
	writableLocalRoots *map[string]string
	gatewayRoots       *map[string]string
	lock               sync.RWMutex
	Client             *http.Client
	Retries            int

	// set to 1 if all writable services are of disk type, otherwise 0
	replicasPerService int
}

// Create a new KeepClient.  This will contact the API server to discover Keep
// servers.
func MakeKeepClient(arv *arvadosclient.ArvadosClient) (*KeepClient, error) {
	var matchTrue = regexp.MustCompile("^(?i:1|yes|true)$")
	insecure := matchTrue.MatchString(os.Getenv("ARVADOS_API_HOST_INSECURE"))

	defaultReplicationLevel := 2
	value, err := arv.Discovery("defaultCollectionReplication")
	if err == nil {
		v, ok := value.(float64)
		if ok && v > 0 {
			defaultReplicationLevel = int(v)
		}
	}

	kc := &KeepClient{
		Arvados:       arv,
		Want_replicas: defaultReplicationLevel,
		Using_proxy:   false,
		Client: &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}}},
		Retries: 2,
	}
	return kc, kc.DiscoverKeepServers()
}

// Put a block given the block hash, a reader, and the number of bytes
// to read from the reader (which must be between 0 and BLOCKSIZE).
//
// Returns the locator for the written block, the number of replicas
// written, and an error.
//
// Returns an InsufficientReplicas error if 0 <= replicas <
// kc.Wants_replicas.
func (kc *KeepClient) PutHR(hash string, r io.Reader, dataBytes int64) (string, int, error) {
	// Buffer for reads from 'r'
	var bufsize int
	if dataBytes > 0 {
		if dataBytes > BLOCKSIZE {
			return "", 0, OversizeBlockError
		}
		bufsize = int(dataBytes)
	} else {
		bufsize = BLOCKSIZE
	}

	t := streamer.AsyncStreamFromReader(bufsize, HashCheckingReader{r, md5.New(), hash})
	defer t.Close()

	return kc.putReplicas(hash, t, dataBytes)
}

// PutHB writes a block to Keep. The hash of the bytes is given in
// hash, and the data is given in buf.
//
// Return values are the same as for PutHR.
func (kc *KeepClient) PutHB(hash string, buf []byte) (string, int, error) {
	t := streamer.AsyncStreamFromSlice(buf)
	defer t.Close()
	return kc.putReplicas(hash, t, int64(len(buf)))
}

// PutB writes a block to Keep. It computes the hash itself.
//
// Return values are the same as for PutHR.
func (kc *KeepClient) PutB(buffer []byte) (string, int, error) {
	hash := fmt.Sprintf("%x", md5.Sum(buffer))
	return kc.PutHB(hash, buffer)
}

// PutR writes a block to Keep. It first reads all data from r into a buffer
// in order to compute the hash.
//
// Return values are the same as for PutHR.
//
// If the block hash and data size are known, PutHR is more efficient.
func (kc *KeepClient) PutR(r io.Reader) (locator string, replicas int, err error) {
	if buffer, err := ioutil.ReadAll(r); err != nil {
		return "", 0, err
	} else {
		return kc.PutB(buffer)
	}
}

// Get() retrieves a block, given a locator. Returns a reader, the
// expected data length, the URL the block is being fetched from, and
// an error.
//
// If the block checksum does not match, the final Read() on the
// reader returned by this method will return a BadChecksum error
// instead of EOF.
func (kc *KeepClient) Get(locator string) (io.ReadCloser, int64, string, error) {
	var errs []string

	tries_remaining := 1 + kc.Retries
	serversToTry := kc.getSortedRoots(locator)
	var retryList []string

	for tries_remaining > 0 {
		tries_remaining -= 1
		retryList = nil

		for _, host := range serversToTry {
			url := host + "/" + locator

			req, err := http.NewRequest("GET", url, nil)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", url, err))
				continue
			}
			req.Header.Add("Authorization", fmt.Sprintf("OAuth2 %s", kc.Arvados.ApiToken))
			resp, err := kc.Client.Do(req)
			if err != nil {
				// Probably a network error, may be transient,
				// can try again.
				errs = append(errs, fmt.Sprintf("%s: %v", url, err))
				retryList = append(retryList, host)
			} else if resp.StatusCode != http.StatusOK {
				respbody, _ := ioutil.ReadAll(&io.LimitedReader{resp.Body, 4096})
				resp.Body.Close()
				errs = append(errs, fmt.Sprintf("%s: %d %s",
					url, resp.StatusCode, bytes.TrimSpace(respbody)))

				if resp.StatusCode >= 500 {
					// Server side failure, may be
					// transient, can try again.
					retryList = append(retryList, host)
				}
			} else {
				// Success.
				return HashCheckingReader{
					Reader: resp.Body,
					Hash:   md5.New(),
					Check:  locator[0:32],
				}, resp.ContentLength, url, nil
			}
		}
		serversToTry = retryList
	}
	log.Printf("DEBUG: GET %s failed: %v", locator, errs)

	if len(retryList) > 0 {
		// There was at least one failure to get a final answer
		return nil, 0, "", KeepServerError
	} else {
		// Ever server returned a 4xx error
		return nil, 0, "", BlockNotFound
	}
}

// Ask() verifies that a block with the given hash is available and
// readable, according to at least one Keep service. Unlike Get, it
// does not retrieve the data or verify that the data content matches
// the hash specified by the locator.
//
// Returns the data size (content length) reported by the Keep service
// and the URI reporting the data size.
func (kc *KeepClient) Ask(locator string) (int64, string, error) {
	for _, host := range kc.getSortedRoots(locator) {
		url := host + "/" + locator
		req, err := http.NewRequest("HEAD", url, nil)
		if err != nil {
			continue
		}
		req.Header.Add("Authorization", fmt.Sprintf("OAuth2 %s", kc.Arvados.ApiToken))
		if resp, err := kc.Client.Do(req); err == nil && resp.StatusCode == http.StatusOK {
			return resp.ContentLength, url, nil
		}
	}
	return 0, "", BlockNotFound
}

// GetIndex retrieves a list of blocks stored on the given server whose hashes
// begin with the given prefix. The returned reader will return an error (other
// than EOF) if the complete index cannot be retrieved.
//
// This is meant to be used only by system components and admin tools.
// It will return an error unless the client is using a "data manager token"
// recognized by the Keep services.
func (kc *KeepClient) GetIndex(keepServiceUUID, prefix string) (io.Reader, error) {
	url := kc.LocalRoots()[keepServiceUUID]
	if url == "" {
		return nil, ErrNoSuchKeepServer
	}

	url += "/index"
	if prefix != "" {
		url += "/" + prefix
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Add("Authorization", fmt.Sprintf("OAuth2 %s", kc.Arvados.ApiToken))
	resp, err := kc.Client.Do(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Got http status code: %d", resp.StatusCode)
	}

	var respBody []byte
	respBody, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Got index; verify that it is complete
	// The response should be "\n" if no locators matched the prefix
	// Else, it should be a list of locators followed by a blank line
	if !bytes.Equal(respBody, []byte("\n")) && !bytes.HasSuffix(respBody, []byte("\n\n")) {
		return nil, ErrIncompleteIndex
	}

	// Got complete index; strip the trailing newline and send
	return bytes.NewReader(respBody[0 : len(respBody)-1]), nil
}

// LocalRoots() returns the map of local (i.e., disk and proxy) Keep
// services: uuid -> baseURI.
func (kc *KeepClient) LocalRoots() map[string]string {
	kc.lock.RLock()
	defer kc.lock.RUnlock()
	return *kc.localRoots
}

// GatewayRoots() returns the map of Keep remote gateway services:
// uuid -> baseURI.
func (kc *KeepClient) GatewayRoots() map[string]string {
	kc.lock.RLock()
	defer kc.lock.RUnlock()
	return *kc.gatewayRoots
}

// WritableLocalRoots() returns the map of writable local Keep services:
// uuid -> baseURI.
func (kc *KeepClient) WritableLocalRoots() map[string]string {
	kc.lock.RLock()
	defer kc.lock.RUnlock()
	return *kc.writableLocalRoots
}

// SetServiceRoots updates the localRoots and gatewayRoots maps,
// without risk of disrupting operations that are already in progress.
//
// The KeepClient makes its own copy of the supplied maps, so the
// caller can reuse/modify them after SetServiceRoots returns, but
// they should not be modified by any other goroutine while
// SetServiceRoots is running.
func (kc *KeepClient) SetServiceRoots(newLocals, newWritableLocals map[string]string, newGateways map[string]string) {
	locals := make(map[string]string)
	for uuid, root := range newLocals {
		locals[uuid] = root
	}

	writables := make(map[string]string)
	for uuid, root := range newWritableLocals {
		writables[uuid] = root
	}

	gateways := make(map[string]string)
	for uuid, root := range newGateways {
		gateways[uuid] = root
	}

	kc.lock.Lock()
	defer kc.lock.Unlock()
	kc.localRoots = &locals
	kc.writableLocalRoots = &writables
	kc.gatewayRoots = &gateways
}

// getSortedRoots returns a list of base URIs of Keep services, in the
// order they should be attempted in order to retrieve content for the
// given locator.
func (kc *KeepClient) getSortedRoots(locator string) []string {
	var found []string
	for _, hint := range strings.Split(locator, "+") {
		if len(hint) < 7 || hint[0:2] != "K@" {
			// Not a service hint.
			continue
		}
		if len(hint) == 7 {
			// +K@abcde means fetch from proxy at
			// keep.abcde.arvadosapi.com
			found = append(found, "https://keep."+hint[2:]+".arvadosapi.com")
		} else if len(hint) == 29 {
			// +K@abcde-abcde-abcdeabcdeabcde means fetch
			// from gateway with given uuid
			if gwURI, ok := kc.GatewayRoots()[hint[2:]]; ok {
				found = append(found, gwURI)
			}
			// else this hint is no use to us; carry on.
		}
	}
	// After trying all usable service hints, fall back to local roots.
	found = append(found, NewRootSorter(kc.LocalRoots(), locator[0:32]).GetSortedRoots()...)
	return found
}

type Locator struct {
	Hash  string
	Size  int      // -1 if data size is not known
	Hints []string // Including the size hint, if any
}

func (loc *Locator) String() string {
	s := loc.Hash
	if len(loc.Hints) > 0 {
		s = s + "+" + strings.Join(loc.Hints, "+")
	}
	return s
}

var locatorMatcher = regexp.MustCompile("^([0-9a-f]{32})([+](.*))?$")

func MakeLocator(path string) (*Locator, error) {
	sm := locatorMatcher.FindStringSubmatch(path)
	if sm == nil {
		return nil, InvalidLocatorError
	}
	loc := Locator{Hash: sm[1], Size: -1}
	if sm[2] != "" {
		loc.Hints = strings.Split(sm[3], "+")
	} else {
		loc.Hints = []string{}
	}
	if len(loc.Hints) > 0 {
		if size, err := strconv.Atoi(loc.Hints[0]); err == nil {
			loc.Size = size
		}
	}
	return &loc, nil
}
