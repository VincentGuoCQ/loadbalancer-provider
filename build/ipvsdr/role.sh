#!/bin/bash

ROLE=$1
INSTANCE=$2

echo "${ROLE}" > /etc/keepalived/role_${INSTANCE}.conf