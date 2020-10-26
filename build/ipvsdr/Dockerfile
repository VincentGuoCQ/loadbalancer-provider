FROM cargo.caicloud.xyz/release/lvs:alpine3.7-1
# Consider fully before changing the base image:
# Changing the distribution or version of iptables/keepalived in base image may cause compatibility problem.

COPY bin/ipvsdr /root/ipvsdr
COPY build/ipvsdr/keepalived.tmpl /root/keepalived.tmpl
COPY build/ipvsdr/keepalived.conf /etc/keepalived/keepalived.conf
COPY build/ipvsdr/role.sh /etc/keepalived/role.sh
RUN  chmod +x /etc/keepalived/role.sh
ENTRYPOINT ["/root/ipvsdr", "--debug"]
