package f5lb

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	log "k8s.io/klog"

	"github.com/caicloud/clientset/kubernetes"
	lbapi "github.com/caicloud/clientset/pkg/apis/loadbalance/v1alpha2"
	"github.com/caicloud/loadbalancer-provider/core/provider"
	core "github.com/caicloud/loadbalancer-provider/core/provider"
	"github.com/caicloud/loadbalancer-provider/pkg/version"
	v1 "k8s.io/api/core/v1"
	v1beta1 "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

/*
Ingress Status:
	annotation.specVersion: N // set by WEB
	annotation.statusVersion: M // set by provider
	annotation.statusMessage: Message // set by provider

	if N > M, then Web display "Updating"
	if N == M && Message == "ok", then Web display "Success"
	if N == M && Message != "ok", then Web display "Failed" and Provider re-sync.

LB Status:
lb.Status.providersStatuses.externallb{
	value1,
	value2,
	metadata map[string]string
}

// lb.f5.status + message
// lb.f5.status + message

#### Ingress
```
  "ingress.Metadata.Annotations": {
    "loadbalance.caicloud.io/dnsInfo": "[]Record{}"    // related dns info
    "loadbalance.caicloud.io/statusMessage": "string"  // update by provider
  }
```
*/

const (
	lbDNSDevicesKey = lbapi.GroupName + "/dns"

	ingressDNSInfoKey    = lbapi.GroupName + "/dnsInfo"
	ingressStatusMessage = lbapi.GroupName + "/statusMessage"
	ingressProviderID    = lbapi.GroupName + "/statusProviderID"

	lbDeviceConfigMap      = "loadbalance-devices-cm"
	deviceTypeDNS          = "DNS"
	deviceTypeF5           = "F5"
	deviceTypeF5SubtypeLTM = "F5-LTM"
	deviceTypeF5SubtypeDNS = "F5-DNS"
	statusOK               = "ok"
)

// LBClient ...
type LBClient interface {
	SetListers(core.StoreLister)
	DeleteLB(lb *lbapi.LoadBalancer) error
	EnsureLB(lb *lbapi.LoadBalancer, tcp *v1.ConfigMap) error
	EnsureIngress(lb *lbapi.LoadBalancer, ing *v1beta1.Ingress, ings []*v1beta1.Ingress) error
	DeleteIngress(lb *lbapi.LoadBalancer, ing *v1beta1.Ingress, ings []*v1beta1.Ingress) error
}

// DNSClient ...
type DNSClient interface {
	EnsureDNSRecords(addList, deleteList *dnsInfoList, l47 string, syncAll bool) error
	EnsureIngress(dns *dnsInfo) error
	DeleteIngress(dns *dnsInfo) error
}

const (
	phaseUninitiaized = "Uninitiaized"
	phaseRunning      = "Running"
	phaseDeleted      = "Deleted"
)

// Provider ...
type Provider struct {
	storeLister core.StoreLister
	clientset   *kubernetes.Clientset
	client      LBClient

	dnsClients map[string]DNSClient

	loadBalancerNamespace string
	loadBalancerName      string

	phase string

	lb        *lbapi.LoadBalancer
	tcpCM     *v1.ConfigMap
	startTime string
}

func getDevices(clientset *kubernetes.Clientset, lb *lbapi.LoadBalancer) (LBClient, map[string]DNSClient, error) {

	lbnamespace := lb.Namespace
	lbname := lb.Name

	devices := []string{lb.Spec.Providers.F5.Name}
	if s := lb.Annotations[lbDNSDevicesKey]; s != "" {
		devices = append(devices, strings.Split(s, ",")...)
	}

	cm, err := clientset.CoreV1().ConfigMaps("kube-system").Get(lbDeviceConfigMap, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	var f5ltm LBClient
	dnsDevices := make(map[string]DNSClient)

	log.Infof("lb has devices: %s, %v", devices, len(devices))
	for _, d := range devices {
		value, ok := cm.Data[d]
		if !ok {
			return nil, nil, fmt.Errorf("Device %s not found in cm", d)
		}
		var device provider.Device
		err := json.Unmarshal([]byte(value), &device)
		if err != nil {
			return nil, nil, err
		}
		if _, ok := dnsDevices[device.Name]; ok {
			continue
		}
		if device.Type == deviceTypeF5 && device.SubType == deviceTypeF5SubtypeLTM {
			f5ltm, err = NewF5LTMClient(device, lbnamespace, lbname)
			if err != nil {
				return nil, nil, err
			}
		}

		if device.Type == deviceTypeF5 && device.SubType == deviceTypeF5SubtypeDNS {
			c, err := newF5DNSClient(device, lbnamespace, lbname)
			if err != nil {
				return nil, nil, err
			}
			dnsDevices[device.Name] = c
		} else if device.Type == deviceTypeDNS {
			c, err := newInfobloxClient(device, lbnamespace, lbname)
			if err != nil {
				return nil, nil, err
			}
			dnsDevices[device.Name] = c
		}
	}
	return f5ltm, dnsDevices, nil
}

// New ...
func New(clientset *kubernetes.Clientset, lb *lbapi.LoadBalancer) (*Provider, error) {

	lbclient, dnsClients, err := getDevices(clientset, lb)
	if err != nil {
		log.Errorf("Failed to get devices %v", err)
		return nil, err
	}

	a := &Provider{
		clientset:             clientset,
		client:                lbclient,
		dnsClients:            dnsClients,
		loadBalancerName:      lb.Name,
		loadBalancerNamespace: lb.Namespace,
		phase:                 phaseUninitiaized,
		lb:                    nil,
		startTime:             time.Now().Format("20060102-15:04:05"),
	}

	if lb.DeletionTimestamp != nil {
		a.phase = phaseDeleted
	}

	return a, nil
}

// WatchKinds ..
func (p *Provider) WatchKinds() []core.QueueObjectKind {
	return []core.QueueObjectKind{core.QueueObjectLoadbalancer, core.QueueObjectConfigmap, core.QueueObjectNode, core.QueueObjectIngress}
}

func (p *Provider) isConfigMapUpdate(cur *v1.ConfigMap) bool {
	old := p.tcpCM
	if old == nil {
		return true
	}
	if old.ResourceVersion >= cur.ResourceVersion {
		return false
	}

	oldDNSInfo := ""
	newDNSInfo := ""
	if old.Annotations != nil {
		oldDNSInfo = old.Annotations[ingressDNSInfoKey]
	}
	if cur.Annotations != nil {
		newDNSInfo = cur.Annotations[ingressDNSInfoKey]
	}

	if !reflect.DeepEqual(old.Data, cur.Data) ||
		oldDNSInfo != newDNSInfo {
		return true
	}

	return false
}

func (p *Provider) isLBNeedUpdate(cur *lbapi.LoadBalancer) bool {
	old := p.lb
	if old == nil {
		return true
	}
	if old.ResourceVersion >= cur.ResourceVersion {
		return false
	}

	// ignore change of status
	if !reflect.DeepEqual(old.Spec, cur.Spec) ||
		!reflect.DeepEqual(old.DeletionTimestamp, cur.DeletionTimestamp) {
		return true
	}

	return false
}

func (p *Provider) updatePhase(lb *lbapi.LoadBalancer) error {
	var err error
	if p.phase == phaseUninitiaized {
		if lb.DeletionTimestamp == nil {
			err = p.onUpdateLB(core.QueueObjectEventAdd, lb, nil)
			if err != nil {
				return err
			}
		}
		p.phase = phaseRunning
	}
	if lb == nil || lb.DeletionTimestamp != nil {
		p.phase = phaseDeleted
	}
	return nil
}

// OnUpdate ...
func (p *Provider) OnUpdate(o *core.QueueObject, lb *lbapi.LoadBalancer, tcpCM *v1.ConfigMap, udpCM *v1.ConfigMap) error {
	var err error
	oldPhase := p.phase

	defer func() {
		if o.Kind == core.QueueObjectLoadbalancer || oldPhase == phaseUninitiaized {
			p.updateLBStatus(lb.Namespace, lb.Name, err)
		}
	}()

	err = p.updatePhase(lb)
	if err != nil {
		log.Errorf("Failed to update phase %s before update: %v", p.phase, err)
		return err
	}

	if o.Kind == core.QueueObjectLoadbalancer {
		lb = lb.DeepCopy()
		tcpCM = tcpCM.DeepCopy()
		if p.isLBNeedUpdate(lb) || p.isConfigMapUpdate(tcpCM) {
			err = p.onUpdateLB(o.Event, lb, tcpCM)
			if err != nil {
				return err
			}
			p.onUpdateLBDNS(o, tcpCM)
		}

		// cache new lb
		p.lb = lb
		p.tcpCM = tcpCM
	}

	if o.Kind == core.QueueObjectIngress {
		ing := o.Object.(*v1beta1.Ingress)
		if !p.isIngressNeedUpdate(o.Event, ing) {
			return nil
		}
		var ingErr error
		defer func() {
			msg := statusOK
			if ingErr != nil {
				msg = ingErr.Error()
			}
			p.updateIngressStatus(ing.Namespace, ing.Name, msg)
		}()

		ingErr = p.onUpdateIngress(o, lb)
		if ingErr != nil {
			return ingErr
		}
		ingErr = p.onUpdateIngressDNS(o)
		if ingErr != nil {
			return ingErr
		}
	}

	return nil
}

func (p *Provider) isIngressNeedUpdate(event core.QueueObjectEvent, ing *v1beta1.Ingress) bool {
	// not update if 1. has message and message is "" 2. provider restart
	if ing.Annotations[ingressProviderID] != p.startTime {
		return true
	}

	// attention: DeletionTimestamp does not always have value when ing is deleted, so we also test the event
	if ing.DeletionTimestamp != nil || event == core.QueueObjectEventDelete {
		return true
	}

	msg, has := ing.Annotations[ingressStatusMessage]
	if !has {
		return true
	}
	if msg != statusOK {
		return true
	}
	return false
}

func (p *Provider) updateIngressStatus(namespace, name string, msg string) {
	ing, err := p.clientset.ExtensionsV1beta1().Ingresses(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return
		}
		log.Errorf("Failed to get ingress %s/%s when update status %v", namespace, name, err)
		return
	}

	statusMessge := msg

	if ing.Annotations[ingressStatusMessage] == statusMessge && ing.Annotations[ingressProviderID] == p.startTime {
		return
	}

	ing.Annotations[ingressStatusMessage] = statusMessge
	ing.Annotations[ingressProviderID] = p.startTime
	log.Infof("Update ingress %s/%s providerid: %s, status: %s", ing.Namespace, ing.Name, p.startTime, statusMessge)
	_, err = p.clientset.ExtensionsV1beta1().Ingresses(namespace).Update(ing)
	if err != nil {
		log.Errorf("Failed to update ingress %s/%s, error: %v", namespace, name, err)
	}
}

// Start ...
func (p *Provider) Start() {
	log.Infof("Startting provider ns %s name %s", p.loadBalancerNamespace, p.loadBalancerName)
}

// Stop ...
func (p *Provider) Stop() error {
	log.Infof("end provider ...")
	return nil
}

// Info ...
func (p *Provider) Info() core.Info {
	info := version.Get()
	return core.Info{
		Name:      "f5lb",
		Version:   info.Version,
		GitCommit: info.GitCommit,
		GitRemote: info.GitRemote,
	}
}

// WaitForStart waits for ipvsdr fully run
func (p *Provider) WaitForStart() bool {
	return true
}

// SetListers sets the configured store listers in the generic ingress controller
func (p *Provider) SetListers(lister core.StoreLister) {
	p.storeLister = lister
	p.client.SetListers(lister)
}

func (p *Provider) onUpdateLB(e core.QueueObjectEvent, lb *lbapi.LoadBalancer, tcp *v1.ConfigMap) error {
	var err error
	if e == core.QueueObjectEventDelete || p.phase == phaseDeleted {
		err = p.client.DeleteLB(lb)
	} else {
		err = p.client.EnsureLB(lb, tcp)
	}
	return err
}

func (p *Provider) onUpdateIngress(o *core.QueueObject, lb *lbapi.LoadBalancer) error {

	selector := labels.Set{lbapi.LabelKeyCreatedBy: fmt.Sprintf("%s.%s", p.loadBalancerNamespace, p.loadBalancerName)}.AsSelector()
	ings, err := p.storeLister.Ingress.List(selector)
	if err != nil {
		log.Errorf("Failed to list ingress")
		return err
	}

	ing := o.Object.(*v1beta1.Ingress)
	if o.Event == core.QueueObjectEventDelete || p.phase == phaseDeleted {
		err = p.client.DeleteIngress(lb, ing, ings)
	} else {
		err = p.client.EnsureIngress(lb, ing, ings)
	}
	return err
}

// onUpdateLBDNS to all dns record by lb
func (p *Provider) onUpdateLBDNS(o *core.QueueObject, tcpCM *v1.ConfigMap) {
	if o.Kind != core.QueueObjectLoadbalancer {
		return
	}

	var err error
	if p.phase != phaseDeleted {
		err = p.ensureAllL4DNS(tcpCM)
		if err != nil {
			log.Errorf("Failed to update L4 dns: %v", err)
		}
		unEnsured := p.tcpCM == nil
		if unEnsured { // Only ensure all L7 at first time
			err = p.ensureAllL7DNS(true)
			if err != nil {
				log.Errorf("Failed to update L7 dns: %v", err)
			}
		}
		return
	}

	err = p.ensureAllL4DNS(nil)
	if err != nil {
		log.Errorf("Failed to clean L4 dns: %v", err)
	}
	err = p.ensureAllL7DNS(false)
	if err != nil {
		log.Errorf("Failed to clean L7 dns: %v", err)
	}
}

func (p *Provider) onUpdateIngressDNS(o *core.QueueObject) error {
	var err error
	//ip, user, password, prefix, vsnames := getF5GTMClientInfo()

	ing := o.Object.(*v1beta1.Ingress)

	if o.Event == core.QueueObjectEventDelete || p.phase == phaseDeleted {
		err = p.updateOneIngressDNS(ing, false)
	} else {
		err = p.updateOneIngressDNS(ing, true)
	}
	return err
}

func (p *Provider) updateOneIngressDNS(ing *v1beta1.Ingress, add bool) error {
	s := ing.Annotations[ingressDNSInfoKey]
	if s == "" {
		return nil
	}

	ds, err := getOneIngressDNSRecord(ing)
	if err != nil {
		return err
	}

	for _, d := range ds {
		client, ok := p.dnsClients[d.DNSName]
		if !ok {
			log.Errorf("dns %s not found for ingress %s", d.DNSName, ing.Name)
			err = fmt.Errorf("invalid dns server %s", d.DNSName)
			break
		}

		if add {
			err = client.EnsureIngress(d)
		} else {
			err = client.DeleteIngress(d)
		}
		if err != nil {
			log.Errorf("Failed to update Ingress %s:%v", ing.Name, err)
			break
		}
	}
	return err
}

func (p *Provider) updateConfigMapStatus(tcpCM *v1.ConfigMap, data map[string]string) {
	cmName := tcpCM.Name + "-status"
	cur, err := p.clientset.CoreV1().ConfigMaps(tcpCM.Namespace).Get(cmName, metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			log.Errorf("Failed to get lb %s/%s when update status %v", tcpCM.Namespace, cmName, err)
			return
		}
		cm := &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:   cmName,
				Labels: tcpCM.Labels,
			},
			Data: data,
		}
		log.Infof("About to create ConfigMap %v/%v: %v", tcpCM.Namespace, cmName, data)
		_, err = p.clientset.CoreV1().ConfigMaps(tcpCM.Namespace).Create(cm)
		if err != nil {
			log.Errorf("Failed to create cm status: %v", err)
		}
		return
	}

	cur.Data = data
	log.Infof("About to update ConfigMap %v/%v: %v", tcpCM.Namespace, cmName, data)
	_, err = p.clientset.CoreV1().ConfigMaps(tcpCM.Namespace).Update(cur)
	if err != nil {
		log.Errorf("Failed to update cm status: %v", err)
	}
}

func (p *Provider) updateLBStatus(namespace, name string, e error) {
	curlb, err := p.clientset.LoadbalanceV1alpha2().LoadBalancers(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return
		}
		log.Errorf("Failed to get lb %s/%s when update status %v", namespace, name, err)
		return
	}

	status := "Error"
	statusMessge := statusOK
	if e != nil {
		statusMessge = e.Error()
	}

	finalizers := []string{fmt.Sprintf("%v-provider", name)}

	if p.phase == phaseDeleted && e == nil {
		finalizers = nil
	}

	// update provider status
	if p.phase == phaseRunning && err == nil {
		status = phaseRunning
	}

	// diff, then update lb
	if reflect.DeepEqual(curlb.Finalizers, finalizers) &&
		reflect.DeepEqual(curlb.Status.ProvidersStatuses.F5.Status, status) &&
		reflect.DeepEqual(curlb.Status.ProvidersStatuses.F5.Message, statusMessge) {
		return
	}
	curlb.Finalizers = finalizers
	curlb.Status.ProvidersStatuses.F5.Status = status
	curlb.Status.ProvidersStatuses.F5.Message = statusMessge

	log.Infof("Update lb %s/%s, status: %s, message: %s, finalizer: %v", namespace, name, status, statusMessge, finalizers)
	_, err = p.clientset.LoadbalanceV1alpha2().LoadBalancers(curlb.Namespace).Update(curlb)
	if err != nil {
		log.Errorf("Failed to update lb %s/%s, error: %v", namespace, name, err)
	}

}

func (p *Provider) ensureAllL4DNS(tcpCM *v1.ConfigMap) error {
	old, new, err := getConfigMapDNSRecordChange(p.tcpCM, tcpCM)
	if err != nil {
		log.Errorf("Failed to parse L4 dns record")
		return err
	}

	_ = p.ensureAllDNS(&old, Layer4, false)
	status := p.ensureAllDNS(&new, Layer4, true)

	if tcpCM != nil {
		p.updateConfigMapStatus(tcpCM, status)
	}

	return nil
}

// ensureAllL7DNS to ensure all l7 dns record
func (p *Provider) ensureAllL7DNS(add bool) error {
	selector := labels.Set{lbapi.LabelKeyCreatedBy: fmt.Sprintf("%s.%s", p.loadBalancerNamespace, p.loadBalancerName)}.AsSelector()
	ings, err := p.storeLister.Ingress.List(selector)
	if err != nil {
		log.Errorf("Failed to list ingress")
		return err
	}

	dnsInfos := getIngressDNSRecord(ings)

	status := p.ensureAllDNS(&dnsInfos, Layer7, add)

	if add {
		for k, m := range status {
			ss := strings.SplitN(k, ".", 2)
			p.updateIngressStatus(ss[0], ss[1], m)
		}
	}
	return nil
}

func (p *Provider) ensureAllDNS(dnsInfos *dnsInfoList, l47 string, add bool) map[string]string {
	dlm := make(dnsInfoListMap)
	for _, d := range *dnsInfos {
		dl, ok := dlm[d.DNSName]
		if !ok {
			dl = &dnsInfoList{d}
			dlm[d.DNSName] = dl
			continue
		}

		var dd *dnsInfo
		for _, d2 := range *dl {
			if d2.hostName == d.hostName && d2.Addr == d.Addr {
				dd = d2
				break
			}
		}
		if dd != nil {
			dd.rules = append(dd.rules, d.rules[0])
		} else {
			*dl = append(*dl, d)
			dlm[d.DNSName] = dl
		}
	}

	status := make(map[string]string)

	for device, client := range p.dnsClients {
		var err error
		ds, ok := dlm[device]
		if !ok {
			ds = &dnsInfoList{}
		}
		addList := &dnsInfoList{}
		deleteList := &dnsInfoList{}

		if add {
			addList = ds
		} else {
			deleteList = ds
		}

		err = client.EnsureDNSRecords(addList, deleteList, l47, false)
		s := statusOK
		if err != nil {
			s = err.Error()
		}

		for _, d := range *ds {
			if d.status != "" {
				s = d.status
			}

			for _, p := range d.rules {
				m := make(map[string]string)
				m["host"] = d.hostName
				m["message"] = s
				bs, _ := json.Marshal(m)
				status[p] = string(bs)
			}
		}
	}
	return status
}
