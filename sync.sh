#!/bin/bash
set -xe

cd ~/go/src/github.com/wfunc/util
util_sha=`git rev-parse HEAD`

cd ~/go/src/github.com/codingeasygo/web
web_sha=`git rev-parse HEAD`

cd ~/go/src/github.com/wfunc/bsck
go get github.com/wfunc/util@$util_sha
go get github.com/codingeasygo/web@$web_sha
go mod tidy
