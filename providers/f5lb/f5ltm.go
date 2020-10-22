package f5lb

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	gobigip "github.com/hanxueluo/go-bigip"

	lbapi "github.com/caicloud/clientset/pkg/apis/loadbalance/v1alpha2"
	core "github.com/caicloud/loadbalancer-provider/core/provider"
	coreutil "github.com/caicloud/loadbalancer-provider/core/util"
	v1 "k8s.io/api/core/v1"
	v1beta1 "k8s.io/api/extensions/v1beta1"
	log "k8s.io/klog"
)

/*

lb: kube-system/lb-198807712
F5 resource name:
	virtualServer: $preset
	irule: $preset
	node: ${ip}
	pool: P_Pool_kube-system_lb-198807712_pool

irule:
when HTTP_REQUEST {
  switch [HTTP::host] {
    "yunxiao-test.infinitus.com.cn" { pool T_Pool_yunxiao-test_80_6 }
    "ali-aegis-test.infinitus.com.cn" { pool T_Pool_yunxiao-test_80_6 }
    "paas-qjy-test.infinitus.com.cn" { pool T_Pool_qjy-test_pub_8080_6 }
    "sba-qjy-test.infinitus.com.cn" { pool T_Pool_qjy-test_pub_8080_6 }
    "config-qjy-test.infinitus.com.cn" { pool T_Pool_qjy-test_pub_8080_6 }
    "fxh-qjypoc-test.infinitus.com.cn" { pool T_Pool_fxh-qjypoc-test_7081_6 }
    "fxh-mobile-qjypoc-test.infinitus.com.cn" { pool T_Pool_fxh-mobile-qjypoc-test_7084_6 }
    "scc-test.infinitus.com.cn" { pool T_Pool_scc_pub_80_6 }
    "acs-test.infinitus.com.cn" { pool T_Pool_acs-test_8080_6 }
	}
}
*/

const defaultHTTPPort = "80"
const F5PartitionCommon = "Common"

type f5LTMClient struct {
	f5CommonClient
	storeLister core.StoreLister
	namePrefix  string

	l7vs *core.LTMVirtualServer
	l4vs *core.LTMVirtualServer
}

// NewF5LTMClient ...
func NewF5LTMClient(d core.Device, lbnamespace, lbname string) (LBClient, error) {
	lbclient := &f5LTMClient{
		namePrefix: fmt.Sprintf("%s_%s_", lbnamespace, lbname),
	}

	for i, s := range d.Config.LTMVSList {
		if s.Type == Layer4 {
			lbclient.l4vs = &d.Config.LTMVSList[i]
		} else {
			lbclient.l7vs = &d.Config.LTMVSList[i]
		}
	}

	lbclient.d = d
	err := initF5Client(&lbclient.f5CommonClient)
	if err != nil {
		return nil, err
	}

	for _, ltmvs := range []*core.LTMVirtualServer{lbclient.l7vs, lbclient.l4vs} {
		if ltmvs == nil {
			continue
		}

		log.Infof("Trying f5ltm API call:%s, user:%s, vs:%s, irule: %s", d.ManageAddr, d.Auth.User, ltmvs.VirtualServer, ltmvs.IRule)
		vs, err := lbclient.f5.GetVirtualServer(ltmvs.VirtualServer)
		if err != nil {
			return nil, err
		}
		irule, err := lbclient.f5.IRule(ltmvs.IRule)
		if err != nil {
			return nil, err
		}
		if irule == nil || vs == nil {
			return nil, fmt.Errorf("resource not found: vs:%v,irule:%v", vs, irule)
		}
	}

	return lbclient, nil

}

func (c *f5LTMClient) SetListers(lister core.StoreLister) {
	c.storeLister = lister
}

// DeleteLB...
func (c *f5LTMClient) DeleteLB(lb *lbapi.LoadBalancer) error {
	var err error
	if c.l7vs != nil {
		newRule := c.generateL7Rule([]*v1beta1.Ingress{})
		err = c.ensureIRule(newRule, c.l7vs.IRule)
		if err != nil {
			log.Warningf("Failed to clean irule %s:%v", c.l7vs.IRule, err)
		}
	}

	if c.l4vs != nil {
		newRule := c.generateL4Rule(map[string]string{})
		err = c.ensureIRule(newRule, c.l4vs.IRule)
		if err != nil {
			log.Warningf("Failed to clean irule %s:%v", c.l7vs.IRule, err)
		}
	}

	var poolName, partition string
	if c.l4vs != nil {
		poolName = c.getPoolName(Layer4)
		partition = c.getPartition(Layer4)
		log.Infof("f5.DeletePool /%s/%s", partition, poolName)
		err = c.f5.DeletePool(c.getResourceFullPath(partition, poolName))
		if err != nil {
			log.Warningf("Failed to delete pool %s", poolName)
		}
	}

	if c.l7vs != nil {
		poolName = c.getPoolName(Layer7)
		partition = c.getPartition(Layer7)
		log.Infof("f5.DeletePool /%s/%s", partition, poolName)
		err = c.f5.DeletePool(c.getResourceFullPath(partition, poolName))
		if err != nil {
			log.Warningf("Failed to delete pool %s", poolName)
		}
	}

	/* //do not clean node
	f5nodes, err := c.f5.Nodes()
	if err != nil {
		log.Warningf("Failed to list F5 nodes %v", err)
	}

	if f5nodes != nil {
		for _, f5node := range f5nodes.Nodes {
			if c.isMyResource(f5node.Name) {
				log.Infof("f5.DeleteNode fails %v", err)
				err = c.f5.DeleteNode(f5node.Name)
				log.Errorf("Failed to list F5 nodes %v", err)
			}
		}
	}
	*/

	return nil
}

// EnsureLB...
func (c *f5LTMClient) EnsureLB(lb *lbapi.LoadBalancer, tcp *v1.ConfigMap) error {
	if c.l7vs != nil {
		_, err := c.f5.GetVirtualServer(c.l7vs.VirtualServer)
		if err != nil {
			log.Errorf("Failed to get F5 vs %s, %v", c.l7vs.VirtualServer, err)
			return err
		}
		if err := c.ensureNodeAndPool(Layer7, lb); err != nil {
			return err
		}
	}

	if c.l4vs != nil {
		if err := c.ensureNodeAndPool(Layer4, lb); err != nil {
			return err
		}

		if tcp != nil {
			newRule := c.generateL4Rule(tcp.Data)
			return c.ensureIRule(newRule, c.l4vs.IRule)
		}
	}

	return nil
}

// DeleteIngresse...
func (c *f5LTMClient) DeleteIngress(lb *lbapi.LoadBalancer, ing *v1beta1.Ingress, ings []*v1beta1.Ingress) error {
	return c.EnsureIngress(lb, ing, ings)
}

// EnsureIngress...
func (c *f5LTMClient) EnsureIngress(lb *lbapi.LoadBalancer, ing *v1beta1.Ingress, ings []*v1beta1.Ingress) error {
	if lb == nil || lb.DeletionTimestamp != nil || c.l7vs == nil {
		// DeleteLB will clean irule, so we do nothing here
		log.Infof("Skip EnsureIngress %s because lb is deleted or has no l7vs", ing.Name)
		return nil
	}

	newRule := c.generateL7Rule(ings)
	return c.ensureIRule(newRule, c.l7vs.IRule)
}

func (c *f5LTMClient) generateL7Rule(ings []*v1beta1.Ingress) string {
	itemsMap := make(map[string]bool)

	for _, i := range ings {
		if i == nil || i.DeletionTimestamp != nil {
			continue
		}
		for _, rule := range i.Spec.Rules {
			itemsMap[rule.Host] = true
		}
	}
	items := make([]string, 0, len(itemsMap))
	for h := range itemsMap {
		items = append(items, h)
	}
	sort.Strings(items)

	/*
		ltm rule /Common/ruleA {
			when HTTP_REQUEST {
			  if { [HTTP::host] equals "hnet23.com" } {
			    pool nodepool23
			  } elseif { [HTTP::host] equals "hnet45.com" } {
			    pool nodepool23
			  }
		    }
		}
	*/
	// update irule content
	poolName := c.getPoolName(Layer7)
	newRule := "# written by lb " + c.namePrefix
	newRule += "\nwhen HTTP_REQUEST {\n"
	if len(items) > 0 {
		newRule += "  switch [HTTP::host] {\n"
	}
	for _, domain := range items {
		newRule += fmt.Sprintf("    \"%s\" { pool %s }\n", domain, poolName)
	}
	if len(items) > 0 {
		newRule += "  }\n"
	}
	newRule += "}"

	/* // if-else
	for i, domain := range domains {
		prefix := "  } elseif"
		if i == 0 {
			prefix = "  if"
		}
		newRule += prefix + " { [HTTP::host] equals \"" + domain + "\" } {\n"
		newRule += "    pool " + poolName + "\n"
	}
	if len(domains) > 0 {
		newRule += "  }\n"
	}
	newRule += "}"
	*/
	return newRule
}

func (c *f5LTMClient) generateL4Rule(l4rules map[string]string) string {
	itemsMap := make(map[string]bool)

	for p := range l4rules {
		if _, err := strconv.Atoi(p); err == nil {
			itemsMap[p] = true
		}
	}
	items := make([]string, 0, len(itemsMap))
	for h := range itemsMap {
		items = append(items, h)
	}
	sort.Strings(items)

	/*
		ltm rule /Common/ruleA {
			when HTTP_REQUEST {
			  if { [HTTP::host] equals "hnet23.com" } {
			    pool nodepool23
			  } elseif { [HTTP::host] equals "hnet45.com" } {
			    pool nodepool23
			  }
		    }
		}
	*/
	// update irule content
	poolName := c.getPoolName(Layer4)
	newRule := "# written by lb " + c.namePrefix
	newRule += "\nwhen CLIENT_ACCEPTED {\n"
	if len(items) > 0 {
		newRule += "  switch [TCP::local_port] {\n"
	}
	for _, port := range items {
		newRule += fmt.Sprintf("    \"%s\" { pool %s }\n", port, poolName)
	}
	if len(items) > 0 {
		newRule += "    else { reject }\n"
		newRule += "  }\n"
	}
	newRule += "}"

	return newRule
}

func (c *f5LTMClient) ensureIRule(newRule string, iruleName string) error {
	// get irule
	obj, err := c.f5.IRule(iruleName)
	if obj == nil && err == nil {
		err = fmt.Errorf("iRule %s doesn't exist", iruleName)
	}

	if err != nil {
		log.Errorf("Failed to get irule %s: %v", iruleName, err)
		return err
	}

	if obj.Rule != newRule {
		obj.Rule = newRule
		log.Infof("f5.ModifyIRule %s:\n%v", iruleName, newRule)
		if err := c.f5.ModifyIRule(obj.FullPath, obj); err != nil {
			log.Errorf("Failed to modify irule %s:%v", iruleName, err)
			return err
		}
	}

	return nil
}

func (c *f5LTMClient) getPoolName(l47 string) string {
	return "P_Pool_" + c.namePrefix + l47 + "_pool"
}

func (c *f5LTMClient) getResourceFullPath(partition, name string) string {
	return "/" + partition + "/" + name
}

func (c *f5LTMClient) ensurePool(partition, name, l47 string) error {
	obj, err := c.f5.GetPool(c.getResourceFullPath(partition, name))

	if err != nil {
		log.Errorf("Failed to get %s: %v", name, err)
		return err
	}

	if obj == nil || obj.Partition != partition {
		monitor := c.getResourceFullPath(partition, "gateway_icmp")
		if l47 == Layer7 {
			monitor = c.getResourceFullPath(partition, "http") // f5 default http monitor
		}
		config := &gobigip.Pool{
			Name:      name,
			Monitor:   monitor,
			Partition: partition,
		}
		log.Infof("f5.AddPool %v", config)
		err = c.f5.AddPool(config)
		if err != nil {
			log.Errorf("Failed to create %s: %v", name, err)
			return err
		}
	}
	return nil
}
func (c *f5LTMClient) getPartition(l47 string) string {
	partition := F5PartitionCommon
	vsPath := ""
	if l47 == Layer7 {
		vsPath = c.l7vs.VirtualServer
	} else {
		vsPath = c.l4vs.VirtualServer
	}
	if len(vsPath) == 0 {
		return partition
	}

	sPart := strings.Split(vsPath, "/")
	if len(sPart) != 3 {
		return partition
	}

	partition = sPart[1]

	return partition
}

func (c *f5LTMClient) ensureNodeAndPool(l47 string, lb *lbapi.LoadBalancer) error {
	poolName := c.getPoolName(l47)
	ips := make(map[string]bool)

	for _, n := range lb.Spec.Nodes.Names {
		ip := coreutil.GetNodeIP(c.storeLister.Node, n)
		if ip == "" {
			log.Warningf("Failed to get ip for node %s", n)
			continue
		}
		ips[ip] = false
	}

	f5nodes, err := c.f5.Nodes()
	if err != nil {
		log.Errorf("Failed to list F5 nodes %v", err)
		return err
	}
	partition := c.getPartition(l47)

	for _, f5node := range f5nodes.Nodes {
		if f5node.Name != f5node.Address {
			log.Warningf("node name %s != addr %s in f5", f5node.Name, f5node.Address)
			continue
		}
		if f5node.Partition != partition {
			continue
		}
		if _, ok := ips[f5node.Name]; ok {
			ips[f5node.Name] = true
		}
	}

	for ip, exist := range ips {
		if exist {
			continue
		}
		log.Infof("f5.CreateNode /%v/%v: %v", partition, ip, exist)
		node := &gobigip.Node{
			Name:      ip,
			Address:   ip,
			Partition: partition,
		}
		err := c.f5.AddNode(node)
		if err != nil {
			log.Errorf("Failed to create F5 node %s, %v", ip, err)
			return err
		}
	}
	err = c.ensurePool(partition, poolName, l47)
	if err != nil {
		return err
	}

	poolMembers, err := c.f5.PoolMembers(c.getResourceFullPath(partition, poolName))
	if err != nil {
		log.Errorf("Failed to poolMember for pool %s: %v", poolName, err)
		return err
	}

	poolPort := "0"
	if l47 == Layer7 {
		poolPort = defaultHTTPPort
	}
	foundCount := 0
	newPoolMembers := []gobigip.PoolMember{}
	for ip := range ips {
		memberName := ip + ":" + poolPort
		var pm *gobigip.PoolMember

		for i := range poolMembers.PoolMembers {
			if poolMembers.PoolMembers[i].Name == memberName {
				foundCount++
				break
			}
		}
		pm = &gobigip.PoolMember{
			Name:      memberName,
			Partition: partition,
		}
		newPoolMembers = append(newPoolMembers, *pm)
	}

	if len(poolMembers.PoolMembers) == len(newPoolMembers) &&
		foundCount == len(poolMembers.PoolMembers) {
		return nil
	}

	log.Infof("f5.UpdatePoolMembers ips: %v, found: %v, newPool: %v", ips, foundCount, newPoolMembers)
	err = c.f5.UpdatePoolMembers(c.getResourceFullPath(partition, poolName), &newPoolMembers)
	if err != nil {
		log.Errorf("Failed to update pool member /%s/%s: %v", partition, poolName, err)
	}
	return err

	/* // not clean node
	for k := range removed {
		log.Infof("f5.DeleteNode %v", k)
		err := c.f5.DeleteNode(k)
		if err != nil {
			log.Warningf("ignore failure of deleting node %s from f5", k)
		}
	}
	*/
}
