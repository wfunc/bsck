#!/bin/bash

srv_ver=`git rev-parse --abbrev-ref HEAD`
cat <<EOF > router/version.go
package router

const Version = "$srv_ver"
EOF


cd bsrouter
go build -o .
cd ../

cd bsconsole
go build -o .
cd ../

if [ "$1" == "install" ];then
    cp -f bsconsole/bsconsole /usr/local/bin/bsconsole
    cp -f bsrouter/bsrouter /usr/local/bin/bsrouter
fi
