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

package provider

import (
	"github.com/caicloud/clientset/kubernetes"
	lblisters "github.com/caicloud/clientset/listers/loadbalance/v1alpha2"
	lbapi "github.com/caicloud/clientset/pkg/apis/loadbalance/v1alpha2"
	v1 "k8s.io/api/core/v1"
	v1listers "k8s.io/client-go/listers/core/v1"
	v1beta1listers "k8s.io/client-go/listers/extensions/v1beta1"
)

// Provider holds the methods to handle an Provider backend
type Provider interface {
	// WatchKinds returns kinds which controller should watch
	WatchKinds() []QueueObjectKind
	// Info returns information about the loadbalancer provider
	Info() Info
	// SetListers allows the access of store listers present in the generic controller
	// This avoid the use of the kubernetes client.
	SetListers(StoreLister)
	// OnUpdate callback invoked when loadbalancer changed
	OnUpdate(*QueueObject, *lbapi.LoadBalancer, *v1.ConfigMap, *v1.ConfigMap) error
	// Start starts the loadbalancer provider
	Start()
	// WaitForStart waits for provider fully run
	WaitForStart() bool
	// Stop shuts down the loadbalancer provider
	Stop() error
}

type QueueObjectEvent int32
type QueueObjectKind int32

const (
	QueueObjectEventAdd QueueObjectEvent = iota
	QueueObjectEventUpdate
	QueueObjectEventDelete
)
const (
	QueueObjectLoadbalancer QueueObjectKind = iota
	QueueObjectIngress
	QueueObjectNode
	QueueObjectConfigmap
	QueueObjecSecret
)

type QueueObject struct {
	Event     QueueObjectEvent `json:"event,omitempty"`
	Kind      QueueObjectKind  `json:"kind,omitempty"`
	Namespace string           `json:"namespace,omitempty"`
	Name      string           `json:"name,omitempty"`
	Object    interface{}
}

// Info returns information about the provider.
// This fields contains information that helps to track issues or to
// map the running loadbalancer provider to source code
type Info struct {
	// Name returns the name of the backend implementation
	Name string `json:"name"`
	// Release returns the running version (semver)
	Version string `json:"version"`
	// Build returns information about the git commit
	GitCommit string `json:"gitCommit"`
	// GitRemote return information about the git remote repository
	GitRemote string `json:"gitRemote"`
}

// StoreLister returns the configured store for loadbalancers, nodes
type StoreLister struct {
	LoadBalancer lblisters.LoadBalancerLister
	Ingress      v1beta1listers.IngressLister
	ConfigMap    v1listers.ConfigMapLister
	Node         v1listers.NodeLister
	Secret       v1listers.SecretLister
}

// Configuration contains all the settings required by an LoadBalancer controller
type Configuration struct {
	KubeClient            kubernetes.Interface
	Backend               Provider
	LoadBalancerName      string
	LoadBalancerNamespace string
	TCPConfigMap          string
	UDPConfigMap          string
}

// Following struct is from url:
// https://github.com/caicloud/platform/pull/1175/files

// DeviceList is a collection of devices
type DeviceList struct {
	//Meta  ListMeta `json:"metadata"`
	Items []Device `json:"items"`
}

// get from lb
// Device represents the external device api
type Device struct {

	// the name of device
	Name string `json:"name"`
	// the ip of device
	//IP string `json:"ip"`
	// ManageAddr is the address of device
	ManageAddr string `json:"manageAddr"`
	// type
	Type string `json:"type"`
	// subtype
	SubType string `json:"subType"`
	// description
	//Description string `json:"description"`
	// alias name
	//Alias string `json:"alias"`
	// auth
	Auth DeviceAuth `json:"auth,omitempty"`
	// config
	Config DeviceConfig `json:"config,omitempty"`
	// owners
	//Owners string `json:"owners"`
}

// DeviceAuth represents
type DeviceAuth struct {
	// User
	User string `json:"user,omitempty"`
	// Pass
	Password string `json:"password,omitempty"`
}

// DeviceConfig represents
type DeviceConfig struct {
	// forbiddenList, separated by comma
	ForbiddenList string `json:"forbiddenList,omitempty"`
	// ZoneList
	ZoneList string `json:"zoneList,omitempty"`
	// Environments
	Environments map[string]string `json:"environments,omitempty"`

	// Infoblox DNS
	// APIVersion
	APIVersion string `json:"apiVersion,omitempty"`

	// F5 DNS
	// pool
	//Pool string `json:"pool,omitempty"`
	// PoolPrefix
	PoolPrefix string `json:"poolPrefix,omitempty"`
	// VirtualServerList
	VirtualServerList string `json:"virtualServerList,omitempty"`

	// LTMVSList
	LTMVSList []LTMVirtualServer `json:"ltmVirtualServerList,omitempty"`
}

// LTMVirtualServer ...
type LTMVirtualServer struct {
	// IP
	IP string `json:"ip,omitempty"`
	// IRule
	IRule string `json:"irule,omitempty"`
	// virtual server name
	VirtualServer string `json:"virtualServer,omitempty"`
	// Type L4/L7
	Type string `json:"type,omitempty"`
}

// Record represents a dns record
type Record struct {
	// Host
	Host string `json:"host"`
	// Addr
	Addr string `json:"addr"` // vs or ip
	// DNSName
	DNSName string `json:"dnsName"` // deviceName
	// SubDomainSuffix
	SubDomainSuffix string `json:"subDomainSuffix"` // subDomainSuffix
	// Zone
	Zone string `json:"zone"`
}
