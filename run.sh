#!/bin/bash

docker rm -f bsrouter
docker run -d --restart=always \
    --name bsrouter \
    -p 31103:31103\
    -v /data/bsrouter/conf:/etc/bsrouter -v /data/bsrouter/ssh:/root/.ssh\
    bsrouter:$1 bsrouter