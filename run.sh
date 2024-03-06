#!/bin/sh

if [ $(command -v getprop) ]; then
	echo detected android host, reading libraries from $PWD/lib
	export LD_LIBRARY_PATH=$PWD/lib:$LD_LIBRARY_PATH
fi

echo running viamrtsp
exec ./bin/viamrtsp $@
