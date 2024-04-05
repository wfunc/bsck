#!/bin/bash

srv_ver=$1
if [ "$1" == "" ];then
    srv_ver=`git rev-parse --abbrev-ref HEAD`
fi 
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
