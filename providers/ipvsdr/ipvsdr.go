/*
Copyright 2017 Caicloud authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ipvsdr

import (
	"fmt"
	"net"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"time"

	lbapi "github.com/caicloud/clientset/pkg/apis/loadbalance/v1alpha2"
	"github.com/caicloud/loadbalancer-provider/core/pkg/sysctl"
	core "github.com/caicloud/loadbalancer-provider/core/provider"
	"github.com/caicloud/loadbalancer-provider/pkg/version"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/flowcontrol"
	log "k8s.io/klog"
	"k8s.io/kubernetes/pkg/util/iptables"
	k8sexec "k8s.io/utils/exec"
)

const (
	tableFilter         = "filter"
	tableMangle         = "mangle"
	iptablesOutputChain = "LOADBALANCER-IPVS-DR-OUTPUT"
)

var _ core.Provider = &Provider{}

var (
	// sysctl changes required by keepalived
	sysctlAdjustments = map[string]string{
		// allows processes to bind() to non-local IP addresses
		"net.ipv4.ip_nonlocal_bind": "1",
		// enable connection tracking for LVS connections
		"net.ipv4.vs.conntrack": "1",
		// Reply only if the target IP address is local address configured on the incoming interface.
		"net.ipv4.conf.all.arp_ignore": "1",
		// Always use the best local address for ARP requests sent on interface.
		"net.ipv4.conf.all.arp_announce": "2",
		// Reply only if the target IP address is local address configured on the incoming interface.
		"net.ipv4.conf.lo.arp_ignore": "1",
		// Always use the best local address for ARP requests sent on interface.
		"net.ipv4.conf.lo.arp_announce": "2",
	}
)

// Provider ...
type Provider struct {
	nodeName          string
	reloadRateLimiter flowcontrol.RateLimiter
	keepalived        *keepalived
	storeLister       core.StoreLister
	sysctlDefault     map[string]string
	ipt               iptables.Interface
	ip6t              iptables.Interface
	cfgMD5            string
	cache             struct {
		lb   *lbapi.LoadBalancer
		tcps []string
		udps []string
	}
}

// NewIpvsdrProvider creates a new ipvs-dr LoadBalancer Provider.
func NewIpvsdrProvider(nodeName string) (*Provider, error) {

	execer := k8sexec.New()

	ipvs := &Provider{
		nodeName:          nodeName,
		reloadRateLimiter: flowcontrol.NewTokenBucketRateLimiter(10.0, 10),
		keepalived:        &keepalived{},
		sysctlDefault:     make(map[string]string),
		ipt:               iptables.New(execer, iptables.ProtocolIpv4),
		ip6t:              iptables.New(execer, iptables.ProtocolIpv6),
	}

	err := ipvs.keepalived.Init()
	if err != nil {
		return nil, err
	}

	return ipvs, nil
}

func (p *Provider) getNodeNetSelector(nodeNetSelectors allNodeNetSelector, nodeName string, binds []*lbapi.KeepalivedBind) (*nodeNetSelector, error) {
	nns, ok := nodeNetSelectors[nodeName]
	if ok {
		return nns, nil
	}
	n, err := p.storeLister.Node.Get(nodeName)
	if err != nil {
		return nil, err
	}
	nns = &nodeNetSelector{
		ifaces: make(map[string]bool),
		ips:    make(map[string]string),
	}
	if ip, _ := getK8sNodeIP(n); ip != nil {
		nns.k8sNodeIP = ip.String()
	}

	for _, b := range binds {
		if b.NodeIPAnnotation != "" {
			ip := getK8sNodeMetadataIP(n, b.NodeIPAnnotation)
			if ip != nil {
				nns.ips[b.NodeIPAnnotation] = ip.String()
			}
		} else {
			nns.ifaces[b.Iface] = true
		}
	}
	nodeNetSelectors[nodeName] = nns
	return nns, nil
}

func (p *Provider) registerNodeNetwork(lb *lbapi.LoadBalancer, nns *nodeNetSelector) (allNodeIfaceNetList, bool, error) {
	var updated bool

	myNodeStatus, err := getCurrentNodeIfaceIPs(p.nodeName, nns)
	if err != nil {
		log.Errorf("Failed to getCurrentNodeIfaceIPs %v", err)
		return nil, updated, err
	}

	allNodeIfaceNets := make(allNodeIfaceNetList)
	allNodeIfaceNets[p.nodeName] = myNodeStatus

	old := lb.Status.NodeStatuses
	new := lbapi.NodeStatuses{}

	keys := []string{p.nodeName}
	for _, n := range lb.Spec.Nodes.Names {
		if n == p.nodeName {
			new.Nodes = append(new.Nodes, *myNodeStatus)
			continue
		}
		for idx, v := range old.Nodes {
			if n == v.Name {
				new.Nodes = append(new.Nodes, old.Nodes[idx])
				allNodeIfaceNets[n] = &old.Nodes[idx]
				keys = append(keys, n)
				break
			}
		}
	}

	if reflect.DeepEqual(old, new) {
		log.Infof("no node status changes. nodes: %v", keys)
		return allNodeIfaceNets, updated, nil
	}

	updated = true

	log.Infof("%s: %d->%d/%d nodes change. ready: %v", p.nodeName, len(old.Nodes), len(new.Nodes), len(lb.Spec.Nodes.Names), keys)

	lb.Status.NodeStatuses = new

	_, err = p.storeLister.KubeClient.Custom().LoadbalanceV1alpha2().LoadBalancers(lb.Namespace).Update(lb)
	if err != nil {
		log.Warningf("Failed to update lb %s, error: %v", lb.Name, err)
	}

	return nil, updated, err
}

func (p *Provider) isLBChanged(new *lbapi.LoadBalancer) bool {
	old := p.cache.lb
	equal := old != nil &&
		reflect.DeepEqual(old.Spec.Nodes, new.Spec.Nodes) &&
		reflect.DeepEqual(old.Spec.Providers.Ipvsdr, new.Spec.Providers.Ipvsdr) &&
		reflect.DeepEqual(old.Status.NodeStatuses, new.Status.NodeStatuses) &&
		reflect.DeepEqual(old.Status.ProxyStatus.TCPConfigMap, new.Status.ProxyStatus.TCPConfigMap) &&
		reflect.DeepEqual(old.Status.ProxyStatus.UDPConfigMap, new.Status.ProxyStatus.UDPConfigMap) &&
		reflect.DeepEqual(old.DeletionTimestamp, new.DeletionTimestamp)
	return !equal
}

func (p *Provider) isListenPortChanged(new *lbapi.LoadBalancer) (bool, []string, []string, error) {
	tcpcm, err := p.storeLister.ConfigMap.ConfigMaps(new.Namespace).Get(new.Status.ProxyStatus.TCPConfigMap)
	if err != nil && !k8serrors.IsNotFound(err) {
		log.Errorf("can not get tcp configmap for loadbalancer: %v", err)
		return false, nil, nil, err
	}
	udpcm, err := p.storeLister.ConfigMap.ConfigMaps(new.Namespace).Get(new.Status.ProxyStatus.UDPConfigMap)
	if err != nil && !k8serrors.IsNotFound(err) {
		log.Errorf("can not get udp configmap for loadbalancer: %v", err)
		return false, nil, nil, err
	}

	tcpPorts, udpPorts := core.GetExportedPorts(new, tcpcm, udpcm)

	sort.Strings(tcpPorts)
	sort.Strings(udpPorts)
	change := !reflect.DeepEqual(p.cache.tcps, tcpPorts) || !reflect.DeepEqual(p.cache.udps, udpPorts)
	return change, tcpPorts, udpPorts, nil
}

func (p *Provider) validate(lb *lbapi.LoadBalancer) error {
	if lb.DeletionTimestamp != nil {
		return fmt.Errorf("lb has been deleted, delete time: %v", lb.DeletionTimestamp)
	}
	if lb.Spec.Providers.Ipvsdr == nil {
		return fmt.Errorf("lb.Spec.Providers.Ipvsdr is nil")
	}

	if len(lb.Spec.Nodes.Names) == 0 {
		return fmt.Errorf("lb has no node")
	}

	hasThisNode := false
	for _, n := range lb.Spec.Nodes.Names {
		if n == p.nodeName {
			hasThisNode = true
			break
		}
	}
	if !hasThisNode {
		return fmt.Errorf("this node %s is not in list %v", p.nodeName, lb.Spec.Nodes.Names)
	}

	if err := lbapi.ValidateLoadBalancer(lb); err != nil {
		return fmt.Errorf("invalid loadbalancer: %v", err)
	}
	return nil
}

// OnUpdate ...
func (p *Provider) OnUpdate(lb *lbapi.LoadBalancer) error {
	var err error
	p.reloadRateLimiter.Accept()

	if err = p.validate(lb); err != nil {
		log.Errorf("invalid loadbalancer: %v, err: %v", lb.Spec, err)
		return nil
	}

	ipvs := lb.Spec.Providers.Ipvsdr

	lbChange := p.isLBChanged(lb)
	listenPortChange, tcps, udps, err := p.isListenPortChanged(lb)
	if err != nil {
		return err
	}

	if !lbChange && !listenPortChange {
		log.Info("Skip Update LoadBalancer because nothing changes")
		return nil
	}

	nodeNetSelectors := make(allNodeNetSelector)
	var nns *nodeNetSelector

	binds := getAllBinds(ipvs)
	nns, err = p.getNodeNetSelector(nodeNetSelectors, p.nodeName, binds)
	if err != nil {
		return err
	}

	allNodeIPs, updated, e := p.registerNodeNetwork(lb, nns)
	if updated || e != nil {
		log.Warningf("Give up this change event. nodeStatus updated: %v, err: %v", updated, e)
		if k8serrors.IsConflict(e) {
			// consider current handler finishes successfully, because another change event is on the way
			e = nil
		}
		return e
	}

	//TODO: try to wait for other node

	log.Info("IPVS: updating network config on host")

	for _, node := range lb.Spec.Nodes.Names {
		_, err = p.getNodeNetSelector(nodeNetSelectors, node, binds)
		if err != nil {
			log.Errorf("Failed to get bind on node %s, error: %v", node, err)
			return err
		}
	}
	if lbChange || listenPortChange {
		err = p.onUpdateIPtables(lb, nodeNetSelectors, allNodeIPs, tcps, udps)
		if err != nil {
			return err
		}
	}

	if lbChange {
		err = p.onUpdateKeepalived(lb, nodeNetSelectors, allNodeIPs)
		if err != nil {
			return err
		}
	}

	p.cache.lb = lb
	p.cache.tcps = tcps
	p.cache.udps = udps

	return nil
}

func getVIPs(kl *lbapi.KeepalivedProvider) []string {
	if len(kl.VIPs) > 0 {
		return kl.VIPs
	}
	return []string{kl.VIP}
}

func (p *Provider) getIPs(nodes []string, allbinds allNodeNetSelector, allNodeIPs allNodeIfaceNetList, bind *lbapi.KeepalivedBind) (*ifacePreferredNet, ifacePreferredNetList) {
	var myIface *ifacePreferredNet
	nodeIPs := ifacePreferredNetList{}

	for _, node := range nodes {
		oneNodeIPs, exists := allNodeIPs[node]
		if !exists {
			continue
		}

		nodeIface := getNodeNetwork(allbinds[node], oneNodeIPs.IfaceNetList, bind)
		if nodeIface != nil {
			nodeIPs = append(nodeIPs, nodeIface)
		}
		if node == p.nodeName {
			myIface = nodeIface
		}
	}
	return myIface, nodeIPs
}

func (p *Provider) getKeepalivedConfigBlock(nodes []string, nodeNetSelectors allNodeNetSelector, allNodeIPs allNodeIfaceNetList, kl *lbapi.KeepalivedProvider, priority, vrid int) ([]*vrrpInstance, []*virtualServer, error) {

	myIface, nodeIPs := p.getIPs(nodes, nodeNetSelectors, allNodeIPs, kl.Bind)

	if myIface == nil {
		return nil, nil, fmt.Errorf("Cannot get self iface ")
	}

	if len(nodes) > len(nodeIPs) {
		log.Warningf("Not all node network are ready: (%d/%d) %v", len(nodeIPs), len(nodes), nodeIPs)
	}

	state := "BACKUP"
	if kl.HAMode == lbapi.ActivePassiveHA {
		if nodes[len(nodes)-1] == p.nodeName {
			state = "MASTER"
		}
	}

	ipVersion := ""
	var vis []*vrrpInstance
	var vss []*virtualServer
	for _, vip := range getVIPs(kl) {
		ip := net.ParseIP(vip)
		if ip == nil {
			continue
		}
		// consider at most 2 vips: one is ipv4 and antoher ipv6
		ipVersion2 := getIPVersion(ip)
		if ipVersion2 == ipVersion {
			continue
		}
		ipVersion = ipVersion2

		myIP := myIface.getIP(ipVersion)
		if myIP == "" {
			log.Errorf("Failed to get my IP%s on iface %s for vip %s", ipVersion, myIface.Name, vip)
			continue
		}

		//TODO
		name := myIface.Name + "_" + ipVersion
		name = string(regexp.MustCompile(`\W`).ReplaceAll([]byte(name), []byte("_")))

		var allIPs []string
		for i, iface := range nodeIPs {
			ip := iface.getIP(ipVersion)
			if ip == "" {
				log.Errorf("Failed to get IP%s on node[%d] for vip %s: %v", ipVersion, i, vip, iface)
				continue
			}
			allIPs = append(allIPs, ip)
		}

		vi := &vrrpInstance{
			Name:      name,
			State:     state,
			Vrid:      vrid,
			Priority:  priority,
			Interface: myIface.Name,
			MyIP:      myIP,
			AllIPs:    allIPs,
			VIP:       vip,
		}
		vis = append(vis, vi)

		if kl.HAMode != lbapi.ActivePassiveHA {
			vs := &virtualServer{
				AcceptMark: acceptMark,
				VIP:        vip,
				Scheduler:  string(kl.Scheduler),
				RealServer: allIPs,
			}
			vss = append(vss, vs)
		}
	}
	return vis, vss, nil
}

func (p *Provider) onUpdateKeepalived(lb *lbapi.LoadBalancer, nodeNetSelectors allNodeNetSelector, allNodeIPs allNodeIfaceNetList) error {

	nodes := make([]string, len(lb.Spec.Nodes.Names))
	copy(nodes, lb.Spec.Nodes.Names)
	sort.Strings(nodes)

	vrrid := 110
	if lb.Status.ProvidersStatuses.Ipvsdr != nil && lb.Status.ProvidersStatuses.Ipvsdr.Vrid != nil {
		vrrid = *lb.Status.ProvidersStatuses.Ipvsdr.Vrid
	}
	ipvs := lb.Spec.Providers.Ipvsdr
	prority := getNodePriority(p.nodeName, nodes)

	vis, vss, err := p.getKeepalivedConfigBlock(nodes, nodeNetSelectors, allNodeIPs, &ipvs.KeepalivedProvider, prority, vrrid)
	if err != nil {
		log.Errorf("Error on getKeepalivedConfigBlock for ipvs provider %v", err)
		return err
	}

	for _, spec := range ipvs.Slaves {
		if spec.HAMode != lbapi.ActivePassiveHA {
			log.Warningf("skip one slave provider %v because ActiveActive Mode is only supported for ipvs now", spec.VIPs)
			// if want to support multple ActiveActive providers, acceptMark should be better designed.
			continue
		}

		vrrid = vrrid + 1
		vis2, vss2, err := p.getKeepalivedConfigBlock(nodes, nodeNetSelectors, allNodeIPs, &spec, prority, vrrid)
		if err != nil {
			log.Warningf("Skip getKeepalivedConfigBlock for keepalived provider %v", err)
			continue
		}
		vis = append(vis, vis2...)
		vss = append(vss, vss2...)
	}

	httpPort := core.GetHTTPPort(lb)
	_ = p.keepalived.UpdateConfig(vis, vss, httpPort)

	// check md5
	md5, err := checksum(keepalivedCfg)
	if err == nil && md5 == p.cfgMD5 {
		log.Warning("md5 is not changed", "md5.old:", p.cfgMD5, "md5.new:", md5)
		return nil
	}

	p.cfgMD5 = md5
	err = p.keepalived.Reload()
	if err != nil {
		log.Errorf("reload keepalived error: %v", err)
		return err
	}
	log.Infof("IPVS: keepalived reload, conf md5: %s", p.cfgMD5)

	return nil
}

// Start ...
func (p *Provider) Start() {
	log.Info("Startting ipvs dr provider")

	_ = p.changeSysctl()
	p.ensureChain()
	p.keepalived.Start()
}

// WaitForStart waits for ipvsdr fully run
func (p *Provider) WaitForStart() bool {
	err := wait.Poll(time.Second, 60*time.Second, func() (bool, error) {
		return p.keepalived.isRunning(), nil
	})

	return err == nil
}

// Stop ...
func (p *Provider) Stop() error {
	log.Info("Shutting down ipvs dr provider")

	err := p.resetSysctl()
	if err != nil {
		log.Errorf("reset sysctl error: %v", err)
	}

	p.deleteChain()

	p.keepalived.Stop()

	return nil
}

// Info ...
func (p *Provider) Info() core.Info {
	info := version.Get()
	return core.Info{
		Name:      "ipvsdr",
		Version:   info.Version,
		GitCommit: info.GitCommit,
		GitRemote: info.GitRemote,
	}
}

// SetListers sets the configured store listers in the generic ingress controller
func (p *Provider) SetListers(lister core.StoreLister) {
	p.storeLister = lister
}

func (p *Provider) ensureChain() {
	for _, ipt := range []iptables.Interface{p.ipt, p.ip6t} {
		// create chain
		ae, err := ipt.EnsureChain(tableMangle, iptables.Chain(iptablesChain))
		if err != nil {
			log.Fatalf("unexpected error: %v", err)
		}
		if ae {
			log.Infof("chain %v already existed", iptablesChain)
		}
		// add rule to let all traffic jump to our chain
		_, _ = ipt.EnsureRule(iptables.Append, tableMangle, iptables.ChainPrerouting, "-j", iptablesChain)

		// create chain
		ae, err = ipt.EnsureChain(tableFilter, iptables.Chain(iptablesOutputChain))
		if err != nil {
			log.Fatalf("unexpected error: %v", err)
		}
		if ae {
			log.Infof("chain %v already existed", iptablesOutputChain)
		}

		// add rule to let all traffic jump to our chain
		_, _ = ipt.EnsureRule(iptables.Append, tableFilter, iptables.ChainOutput, "-j", iptablesOutputChain)
	}
}

func (p *Provider) flushChain() {
	log.Info("flush iptables rules", "table:", tableMangle, "chain:", iptablesChain)
	_ = p.ipt.FlushChain(tableMangle, iptables.Chain(iptablesChain))
	_ = p.ip6t.FlushChain(tableMangle, iptables.Chain(iptablesChain))

	_ = p.ipt.FlushChain(tableFilter, iptables.Chain(iptablesOutputChain))
	_ = p.ip6t.FlushChain(tableFilter, iptables.Chain(iptablesOutputChain))
}

func (p *Provider) deleteChain() {
	// flush chain
	p.flushChain()

	for _, ipt := range []iptables.Interface{p.ipt, p.ip6t} {
		// delete jump rule
		_ = ipt.DeleteRule(tableMangle, iptables.ChainPrerouting, "-j", iptablesChain)
		// delete chain
		_ = ipt.DeleteChain(tableMangle, iptablesChain)

		// delete jump rule
		_ = ipt.DeleteRule(tableFilter, iptables.ChainOutput, "-j", iptablesOutputChain)
		// delete chain
		_ = ipt.DeleteChain(tableFilter, iptablesOutputChain)
	}
}

// changeSysctl changes the required network setting in /proc to get
// keepalived working in the local system.
func (p *Provider) changeSysctl() error {
	var err error
	p.sysctlDefault, err = sysctl.BulkModify(sysctlAdjustments)
	return err
}

// resetSysctl resets the network setting
func (p *Provider) resetSysctl() error {
	log.Info("reset sysctl to original value", "defaults:", p.sysctlDefault)
	_, err := sysctl.BulkModify(p.sysctlDefault)
	return err
}

func (p *Provider) appendIptablesMark(vip net.IP, protocol, iface string, mark int, mac string, ports []string) (bool, error) {
	return p.setIptablesMark(iptables.Append, vip, protocol, iface, mark, mac, ports)
}

func (p *Provider) prependIptablesMark(vip net.IP, protocol, iface string, mark int, mac string, ports []string) (bool, error) {
	return p.setIptablesMark(iptables.Prepend, vip, protocol, iface, mark, mac, ports)
}

func (p *Provider) setIptablesMark(position iptables.RulePosition, ip net.IP, protocol, iface string, mark int, mac string, ports []string) (bool, error) {
	ipt := p.ipt
	if getIPVersion(ip) == ipv6Version {
		ipt = p.ip6t
	}
	vip := ip.String()
	if len(ports) == 0 {
		return ipt.EnsureRule(position, tableMangle, iptablesChain, p.buildIptablesArgs(vip, protocol, iface, mark, mac, "")...)
	}
	// iptables: too many ports specified
	// multiport accept max ports number may be 15
	for _, port := range ports {
		_, err := ipt.EnsureRule(position, tableMangle, iptablesChain, p.buildIptablesArgs(vip, protocol, iface, mark, mac, port)...)
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

func (p *Provider) buildIptablesArgs(vip, protocol, iface string, mark int, mac string, port string) []string {
	args := make([]string, 0)
	args = append(args, "-i", iface, "-d", vip, "-p", protocol)
	if port != "" {
		args = append(args, "-m", "multiport", "--dports", port)
	}
	if mac != "" {
		args = append(args, "-m", "mac", "--mac-source", mac)
	}
	args = append(args, "-j", "MARK", "--set-xmark", fmt.Sprintf("%s/%s", strconv.Itoa(mark), mask))
	log.Infof("build iptables args %v", args)
	return args
}

func (p *Provider) onUpdateIPtables(lb *lbapi.LoadBalancer, nodeNetSelectors allNodeNetSelector, allNodeIPs allNodeIfaceNetList, tcps, udps []string) error {
	nodes := lb.Spec.Nodes.Names
	ipvs := lb.Spec.Providers.Ipvsdr
	myIface, nodeIPs := p.getIPs(nodes, nodeNetSelectors, allNodeIPs, ipvs.Bind)

	if myIface == nil {
		return fmt.Errorf("Cannot get self iface ")
	}

	if len(nodes) > len(nodeIPs) {
		log.Warningf("Not all node network are retrieved: %d > %d", len(nodes), len(nodeIPs))
	}

	// flush all rules
	p.flushChain()
	if ipvs.HAMode != lbapi.ActivePassiveHA {
		for _, vip := range getVIPs(&ipvs.KeepalivedProvider) {
			if vip != "" {
				p.ensureIptablesMark(vip, myIface.Name, nodeIPs, tcps, udps)
				p.ensureIptablesOutputDrop(vip, myIface.Name)
			}
		}
	}

	return nil
}

func (p *Provider) ensureIptablesOutputDrop(vip, iface string) {
	// ensure Drop rule after Mark rule

	ipt := p.ipt
	if getIPVersion(net.ParseIP(vip)) == ipv6Version {
		ipt = p.ip6t
	}
	for _, proto := range []string{"udp", "tcp"} {
		rule := []string{
			"-o", iface,
			"-d", vip,
			"-p", proto,
			"-m", "mark", "--mark", fmt.Sprintf("%d/%s", dropMark, mask),
			"-j", "DROP",
		}
		_, _ = ipt.EnsureRule(iptables.Append, tableFilter, iptablesOutputChain, rule...)
	}
}

func (p *Provider) ensureIptablesMark(vip, iface string, nodeIPs ifacePreferredNetList, tcpPorts, udpPorts []string) {
	var ip net.IP
	if ip = net.ParseIP(vip); ip == nil {
		log.Errorf("failed to ensure iptables rules because of invalid vip: %s", vip)
		return
	}

	// Accoding to #19
	// we must add the mark 0 firstly and then prepend mark 1
	// so that

	// all neighbors' rules should be under the basic rules, to override it
	// make sure that all traffics which come from the neighbors will be marked with 0
	// and than lvs will ignore it
	for _, neighbor := range nodeIPs {
		mac := neighbor.Mac
		if mac == "" {
			log.Infof("skip reset iptables mark for %v", neighbor)
			continue
		}
		_, err := p.appendIptablesMark(ip, "ip", iface, dropMark, mac, nil)
		if err != nil {
			log.Errorf("failed to ensure iptables tcp rule, iface:%s, net: %v, mac: %v, mark: %v, err: %v", iface, neighbor, mac, dropMark, err)
		}
	}

	// this two rules must be prepend before mark 0
	// they mark all matched tcp and udp traffics with 1
	mac := ""
	if len(tcpPorts) > 0 {
		_, err := p.prependIptablesMark(ip, "tcp", iface, acceptMark, mac, tcpPorts)
		if err != nil {
			log.Error("error ensure iptables tcp rule for", "tcpPorts:", tcpPorts, "err:", err)
		}
	}
	if len(udpPorts) > 0 {
		_, err := p.prependIptablesMark(ip, "udp", iface, acceptMark, mac, udpPorts)
		if err != nil {
			log.Error("error ensure iptables udp rule for", "udpPorts:", udpPorts, "err:", err)
		}
	}
}
