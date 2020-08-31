package f5lb

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/caicloud/loadbalancer-provider/core/provider"
	v1 "k8s.io/api/core/v1"
	v1beta1 "k8s.io/api/extensions/v1beta1"
	log "k8s.io/klog"
)

type dnsInfo struct {
	provider.Record
	hostName string
	l47      string
	rules    []string
	status   string
}

type dnsInfoList []*dnsInfo
type dnsInfoListMap map[string]*dnsInfoList

func getOneIngressDNSRecord(ing *v1beta1.Ingress) (dnsInfoList, error) {
	var ds dnsInfoList
	if len(ing.Spec.Rules) == 0 {
		log.Warningf("Skip ing %s who has no rules", ing.Name)
		return ds, nil
	}

	s := ing.Annotations[ingressDNSInfoKey]
	if s == "" {
		return ds, nil
	}

	var dnsRecord []provider.Record
	if err := json.Unmarshal([]byte(s), &dnsRecord); err != nil {
		log.Errorf("Failed to Unmarshal ingress %s dnsInfo: %s", ing.Name, s)
		return ds, err
	}

	hostName := ing.Spec.Rules[0].Host

	for _, dns := range dnsRecord {
		d := &dnsInfo{
			hostName: hostName,
			Record:   dns,
			l47:      "l7",
		}
		d.rules = append(d.rules, fmt.Sprintf("%s.%s", ing.Namespace, ing.Name))
		ds = append(ds, d)
	}

	return ds, nil
}
func getIngressDNSRecord(ings []*v1beta1.Ingress) dnsInfoList {
	var ds dnsInfoList
	for _, ing := range ings {
		ds0, _ := getOneIngressDNSRecord(ing)
		if len(ds0) > 0 {
			ds = append(ds, ds0...)
		}
	}

	return ds
}

func getConfigMapDNSRecord(tcpCM *v1.ConfigMap) (dnsInfoList, error) {
	var ds dnsInfoList
	s := tcpCM.Annotations[ingressDNSInfoKey]
	if s == "" {
		return ds, nil
	}

	dnsRecords := make(map[string][]provider.Record)
	if err := json.Unmarshal([]byte(s), &dnsRecords); err != nil {
		log.Errorf("Failed to Unmarshal dnsInfo: %s", s)
		return ds, err
	}

	for k := range tcpCM.Data {
		drs, ok := dnsRecords[k]
		if !ok || len(drs) == 0 {
			continue
		}
		dnsRecord := drs[0]
		d := &dnsInfo{
			Record:   dnsRecord,
			l47:      "l4",
			hostName: dnsRecord.Host,
		}
		d.rules = append(d.rules, k)
		ds = append(ds, d)
	}

	return ds, nil
}

/*
func getIngressDNSRecords(ings []*v1beta1.Ingress) (dnsInfoList, error) {
	var ds dnsInfoList
	for _, ing := range ings {
		dfl, err := getIngressDNSRecord(ing)
		if err != nil {
			log.Errorf("Failed to parse L7 dns record %s: %v", ing.Name, err)
			return ds, err
		}
		ds = append(ds, dfl...)
	}

	return ds, nil
}
*/

func serializeMetadata(lbname string, d *dnsInfo) string {
	return fmt.Sprintf("cpslb;v1;%s;%s;%s;", lbname, d.l47, strings.Join(d.rules, ","))
}

func unserializeMetadata(info string) (string, *dnsInfo) {
	//cpslb;v1;$lb;l7;name1,name2,name3...
	//cpslb;v1;$lb;l4;port1,port2...

	var lbname string
	if !strings.Contains(info, "cpslb;") {
		return lbname, nil
	}
	items := strings.Split(info, ";")
	if len(items) < 5 {
		return lbname, nil
	}
	if items[1] != "v1" {
		return lbname, nil
	}
	lbname = items[2]
	/*
		if items[3] != "l7" && items[3] != "l4" {
			return lbname, nil
		}
	*/
	d := &dnsInfo{}
	d.l47 = items[3]
	d.rules = strings.Split(items[4], ",")
	return lbname, d
}

func (c *f5DNSClient) checkOwner(info string) (bool, *dnsInfo) {
	lb, d := unserializeMetadata(info)
	if lb != c.lbname {
		return false, d
	}
	return true, d
}