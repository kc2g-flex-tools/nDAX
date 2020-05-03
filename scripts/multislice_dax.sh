#!/bin/bash

# Change to give a specific client name to this application
FLEX_STATION=flex_station_name

./nDAX -station ${FLEX_STATION} -slice A -daxch 1 -sink flex.sliceA.rx -source flex.sliceA.tx &
./nDAX -station ${FLEX_STATION} -slice B -daxch 2 -sink flex.sliceB.rx -source flex.sliceB.tx &
./nDAX -station ${FLEX_STATION} -slice C -daxch 3 -sink flex.sliceC.rx -source flex.sliceC.tx &
./nDAX -station ${FLEX_STATION} -slice D -daxch 4 -sink flex.sliceD.rx -source flex.sliceD.tx &