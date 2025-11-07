#!/bin/bash
docker rm -f bsrouter
docker run -d --restart=always \
    --name bsrouter \
    --privileged \
    -p 31103:31103\
    -v ./conf:/etc/bsrouter -v ./ssh:/root/.ssh\
    registry.cyydm.shop/bsrouter:v1.0.1 bsrouter
