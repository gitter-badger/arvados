#!/bin/bash -x

build=$1
file=$2
outputdir=$3

usage() {
    echo "./$0 build_number file_to_parse output_dir"
    echo "this script will use the build output to generate *csv and *txt"
    echo "for jenkins plugin plot https://github.com/jenkinsci/plot-plugin/"
}

if [ $# -ne 3 ]
then
    usage 
    exit 1
fi

if [ ! -e $file ]
then
    usage
    echo "$file doesn't exists! exiting"
    exit 2
fi
if [ ! -w $outputdir ]
then
    usage
    echo "$outputdir isn't writeable! exiting"
    exit 3
fi

#------------------------------
## max lines that a test will output
MAXLINES=1000 

## TODO: check $build and $file make sense

for test in \
 test_Create_and_show_large_collection_with_manifest_text_of_20000000 \
 test_Create,_show,_and_update_description_for_large_collection_with_manifest_text_of_100000 \
 test_Create_one_large_collection_of_20000000_and_one_small_collection_of_10000_and_combine_them
do
(zgrep -i -E -A$MAXLINES "^[A-Za-z0-9]+Test: $test" $file && echo "----") | tail --lines=+3|grep -B$MAXLINES -E "^-*$" -m1 > $outputdir/$test-$build.txt
 result=$?
 if [ $result -eq 0 ]
 then 
   echo processing  $outputdir/$test-$build.txt creating  $outputdir/$test.csv
   echo $(grep ^Completed $outputdir/$test-$build.txt | perl -n -e '/^Completed (.*) in [0-9]+ms.*$/;print "".++$line."-$1,";' | perl -p -e 's/,$//g'|tr " " "_" ) >  $outputdir/$test.csv
   echo $(grep ^Completed $outputdir/$test-$build.txt | perl -n -e '/^Completed.*in ([0-9]+)ms.*$/;print "$1,";' | perl -p -e 's/,$//g' ) >>  $outputdir/$test.csv
   #echo URL=https://ci.curoverse.com/view/job/arvados-api-server/ws/apps/workbench/log/$test-$build.txt/*view*/ >>  $outputdir/$test.properties
 else  
   echo "$test was't found on $file"
 fi
done
