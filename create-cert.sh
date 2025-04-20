#!/bin/bash

# 生成私钥
openssl genpkey -algorithm RSA -out server.key -pkeyopt rsa_keygen_bits:2048

# 自签证书
openssl req -new -x509 -key server.key -out server.crt -days 365 -subj "/CN=localhost"


cp server.key bsrouterCA.key
cp server.key bsrouter.key

cp server.crt bsrouterCA.pem
cp server.crt bsrouter.pem