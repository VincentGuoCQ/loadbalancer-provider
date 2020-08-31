package f5lb

import (
	"encoding/base64"
	"fmt"
	"strings"

	gobigip "github.com/hanxueluo/go-bigip"

	"github.com/caicloud/loadbalancer-provider/core/provider"
	log "k8s.io/klog"
)

type f5DNSClient struct {
	f5CommonClient
	//virtualServers [][]string
	poolPrefix string
	lbname     string
	rule2Host  map[string]string
}
type f5CommonClient struct {
	f5 *gobigip.BigIP
	d  provider.Device
}

func initF5Client(c *f5CommonClient) error {
	if c.f5 == nil {
		bs, err := base64.StdEncoding.DecodeString(c.d.Auth.Password)
		if err != nil {
			return err
		}
		log.Infof("gobigip.NewSession for %s device", c.d.Name)
		f5 := gobigip.NewSession(c.d.ManageAddr, c.d.Auth.User, string(bs), nil)
		c.f5 = f5
	}
	return nil
}

/*
gtm pool /Common/pool_all_3D {
    alternate-mode none
    fallback-mode global-availability
    load-balancing-mode topology
    members {
    ¦   /Common/ct01_beautrio:vs_ct01_beautrio {
    ¦   ¦   order 0
    ¦   }
    ¦   /Common/unicom01_beautrio:vs_unicom01_beautrio {
    ¦   ¦   order 1
    ¦   }
    }
    monitor /Common/gateway_icmp
}
*/

// NewF5DNSClient ...
func newF5DNSClient(d provider.Device, lbnamespace, lbname string) (DNSClient, error) {
	lbclient := &f5DNSClient{
		poolPrefix: d.Config.PoolPrefix,
		lbname:     fmt.Sprintf("%s.%s", lbnamespace, lbname),
		rule2Host:  make(map[string]string),
	}
	lbclient.d = d

	err := initF5Client(&lbclient.f5CommonClient)
	if err != nil {
		return nil, err
	}

	log.Infof("Trying f5gtm list pool API call:%s, user:%s", d.ManageAddr, d.Auth.User)
	_, err = lbclient.f5.GetGTMCNamePools()
	if err != nil {
		return nil, err
	}

	return lbclient, nil
}

func (c *f5DNSClient) EnsureDNSRecords(dnsInfos *dnsInfoList, l47 string) error {
	var err error
	log.Infof("EnsureDNSRecords %s dl: %v", l47, dnsInfos)
	if l47 != "l7" {
		return nil
	}

	for _, d := range *dnsInfos {
		if d.l47 != l47 {
			log.Warningf("skip dnsinfo %+v for f5dns, because l47 not match", d)
			continue
		}
		if d.status == "" {
			err = c.ensureHost(d.hostName, d)
			if err != nil {
				d.status = err.Error()
				continue
			} else {
				c.cacheRuleHost(d, d.hostName)
				d.status = statusOK
			}
		}
	}

	// delete all orphan hostName
	wips, err := c.f5.GetGTMWideIPs()
	if err != nil || wips == nil {
		return err
	}
	for _, wip := range wips.GTMWideIPs {
		thisLB, dd := c.checkOwner(wip.Description)
		if !thisLB || dd.l47 != l47 {
			continue
		}
		found := false
		for _, d := range *dnsInfos {
			if d.hostName == wip.Name {
				found = true
				d.status = statusOK
				break
			}
		}
		if !found {
			log.Warningf("Has orphan wip %v, try to delete it", wip)
			_ = c.deleteHost(wip.Name)
		}
	}
	return nil
}

func (c *f5DNSClient) cacheRuleHost(dns *dnsInfo, hostName string) {
	for _, r := range dns.rules {
		if hostName == "" {
			delete(c.rule2Host, r)
			continue
		}
		c.rule2Host[r] = hostName
	}
}
func (c *f5DNSClient) loadRuleHost(ruleName string) string {
	return c.rule2Host[ruleName]
}
func (c *f5DNSClient) EnsureIngress(dns *dnsInfo) error {
	var err error
	oldHostName := c.loadRuleHost(dns.rules[0])

	// handle hostName change
	if oldHostName != "" && oldHostName != dns.hostName {
		log.Warningf("host name change")
		err = c.deleteHost(oldHostName)
		if err != nil {
			return err
		}
	}

	if dns.hostName != "" {
		err = c.ensureHost(dns.hostName, dns)
		if err != nil {
			return err
		}
		c.cacheRuleHost(dns, dns.hostName)
	}

	return nil
}

func (c *f5DNSClient) DeleteIngress(dns *dnsInfo) error {
	var err error
	if dns.hostName != "" {
		err = c.deleteHost(dns.hostName)
		if err != nil {
			return err
		}
		c.cacheRuleHost(dns, "")
	}
	return nil
}

func (c *f5DNSClient) getPoolName(hostName string) string {
	// name rule:  xxx-yyy-zzz.domain.com -> ${prefix}_zzz_yyy_xxx
	ns := strings.Split(hostName, ".")
	name := ""
	for _, s := range strings.Split(ns[0], "-") {
		name = s + "_" + name
	}
	name = strings.TrimRight(name, "_")
	return c.poolPrefix + "_" + name
}

func (c *f5DNSClient) ensurePool(name string, desc string) (*gobigip.GTMAPool, error) {
	obj, err := c.f5.GetGTMAPool(name)

	if err != nil {
		log.Errorf("Failed to get %s: %v", name, err)
		return nil, err
	}

	if obj == nil {
		config := &gobigip.GTMAPool{
			Name:              name,
			Description:       desc,
			Monitor:           "/Common/gateway_icmp",
			LoadBalancingMode: "topology",
			FallbackMode:      "global-availability",
		}
		log.Infof("f5.AddGTMAPool %v", config)
		err = c.f5.AddGTMAPool(config)
		if err != nil {
			log.Errorf("Failed to create %s: %v", name, err)
			return nil, err
		}
		obj, err = c.f5.GetGTMAPool(name)
		if err != nil {
			log.Errorf("Failed to get %s after create: %v", name, err)
			return nil, err
		}
	}
	return obj, nil
}

func (c *f5DNSClient) deleteHost(hostName string) error {
	wip, err := c.f5.GetGTMWideIP(hostName)
	if err != nil {
		log.Errorf("Failed to GetGTMWideIP %s when delete: %v ", hostName, err)
		return err
	}

	if wip != nil {
		thisLB, _ := c.checkOwner(wip.Description)
		if !thisLB {
			log.Warningf("skip delete dns record because of conflict domain %s on f5", hostName)
			return nil
		}
		//TODO
		//d0.rules
	}

	if wip != nil {
		log.Infof("f5.DeleteGTMWideIP %s", hostName)
		err = c.f5.DeleteGTMWideIP(hostName)
		if err != nil {
			log.Errorf("Failed to DeleteGTMWideIP %s: %v ", hostName, err)
			return err
		}
	}

	poolName := c.getPoolName(hostName)
	pool, err := c.f5.GetGTMAPool(poolName)
	if err != nil {
		log.Errorf("Failed to GetGTMAPool %s when delete: %v ", poolName, err)
		return err
	}
	if pool != nil {
		log.Infof("f5.DeleteGTMPool %s", poolName)
		err = c.f5.DeleteGTMPool(poolName)
		if err != nil {
			log.Warningf("Failed to DeleteGTMPool %s: %v ", poolName, err)
			return err
		}
	}
	return nil
}

func (c *f5DNSClient) ensureHost(hostName string, d *dnsInfo) error {
	var err error
	if d.Zone == "" || !strings.HasSuffix(d.hostName, d.Zone) {
		log.Errorf("DNS record has an invalid zone: %v", d)
		return fmt.Errorf("invalid zone")
	}

	// ensure wide ip
	wip, err := c.f5.GetGTMWideIP(hostName)
	if err != nil {
		log.Errorf("Failed to get WIP %s: %v ", hostName, err)
		return err
	}

	if wip != nil {
		thisLB, _ := c.checkOwner(wip.Description)
		if !thisLB {
			log.Errorf("Failed to ensure host because of conflict domain name %s on f5", hostName)
			return fmt.Errorf("conflict domain name")
		}
	}

	poolName := c.getPoolName(hostName)
	description := serializeMetadata(c.lbname, d)
	// ensure pool
	_, err = c.ensurePool(poolName, description)
	if err != nil {
		return err
	}

	// ensure pool members
	poolMembers, err := c.f5.GetGTMAPoolMembers(poolName)
	if err != nil {
		log.Errorf("Failed to get pool member %s", poolName)
		return err
	}

	deleted := []gobigip.GTMAPoolMember{}
	var added [][]string
	for _, vsname := range strings.Split(d.Addr, ",") {
		sps := strings.SplitN(vsname, ":", 2)
		if len(sps) != 2 {
			log.Errorf("Incorrect vs name format: %s", vsname)
			continue
		}
		added = append(added, sps)
	}
	for j, pm := range poolMembers.GTMAPoolMembers {
		found := false
		for i, a := range added {
			fullPath := a[0] + ":" + a[1]
			if pm.FullPath == fullPath {
				// delete added[i]
				added[i] = added[len(added)-1]
				added = added[:len(added)-1]
				found = true
				break
			}
		}
		if !found {
			deleted = append(deleted, poolMembers.GTMAPoolMembers[j])
		}
	}

	log.Infof("updating pool member add: %v, delete: %v", added, deleted)
	for _, a := range added {
		log.Infof("f5.CreateGTMAPoolMember %s: %s:%s", poolName, a[0], a[1])
		err = c.f5.CreateGTMAPoolMember(poolName, a[0], a[1])
		if err != nil {
			log.Errorf("Failed to CreateGTMAPoolMember %s: %s:%s: %v ", poolName, a[0], a[1], err)
			return err
		}
	}

	for _, d := range deleted {
		log.Infof("f5.DeleteGTMAPoolMember %s: %s:%s", poolName, d.Name, d.SubPath)
		err = c.f5.DeleteGTMAPoolMember(poolName, d.Name, d.SubPath)
		if err != nil {
			log.Errorf("Failed to DeleteGTMAPoolMember %s: %s:%s: %v ", poolName, d.Name, d.SubPath, err)
			return err
		}
	}

	pools := []gobigip.GTMWideIPPool{
		{Name: poolName},
	}
	if wip == nil {
		o := gobigip.GTMWideIP{
			Name:        hostName,
			Pools:       &pools,
			Description: description,
		}
		log.Infof("f5.AddGTMWideIP %s, %+v", hostName, pools)
		err = c.f5.AddGTMWideIP(&o)
		if err != nil {
			log.Errorf("Failed to AddGTMWideIP %s: %v ", hostName, err)
		}
		return err
	}

	if (wip.Pools != nil && len(*wip.Pools) == 1 && (*wip.Pools)[0].Name == poolName) &&
		wip.Description == description {
		return nil
	}

	log.Infof("f5.ModifyGTMWideIP %s, desc:%s->%s, pool:%+v->%+v",
		hostName, wip.Description, description, wip.Pools, pools)

	wip.Pools = &pools
	wip.Description = description
	err = c.f5.ModifyGTMWideIP(hostName, wip)
	if err != nil {
		log.Errorf("Failed to ModifyGTMWideIP %s: %v ", hostName, err)
		return err
	}

	return nil
}
