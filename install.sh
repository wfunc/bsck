sys=`uname`
case $sys in
Linux)
    go build -o /usr/local/bin/bsrouter github.com/wfunc/bsck/bsrouter 
    go build -o /usr/local/bin/bsconsole github.com/wfunc/bsck/bsconsole 
;;
Darwin)
    go install github.com/wfunc/bsck/bsrouter 
    go install github.com/wfunc/bsck/bsconsole 
    ln -sf `pwd`/bsconsole/bs-scp.sh $GOPATH/bin/bs-scp
    ln -sf `pwd`/bsconsole/bs-sftp.sh $GOPATH/bin/bs-sftp
    ln -sf `pwd`/bsconsole/bs-ssh.sh $GOPATH/bin/bs-ssh
    ln -sf $GOPATH/bin/bsconsole $GOPATH/bin/bs-ping
    ln -sf $GOPATH/bin/bsconsole $GOPATH/bin/bs-state
    ln -sf $GOPATH/bin/bsconsole $GOPATH/bin/bs-bash
    ln -sf $GOPATH/bin/bsconsole $GOPATH/bin/bs-sh
;;
esac