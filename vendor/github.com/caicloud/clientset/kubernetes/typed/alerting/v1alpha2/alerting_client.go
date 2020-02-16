/*
Copyright 2020 caicloud authors. All rights reserved.
*/

// Code generated by client-gen. DO NOT EDIT.

package v1alpha2

import (
	"github.com/caicloud/clientset/kubernetes/scheme"
	v1alpha2 "github.com/caicloud/clientset/pkg/apis/alerting/v1alpha2"
	serializer "k8s.io/apimachinery/pkg/runtime/serializer"
	rest "k8s.io/client-go/rest"
)

type AlertingV1alpha2Interface interface {
	RESTClient() rest.Interface
	AlertingRulesGetter
	AlertingSubRulesGetter
}

// AlertingV1alpha2Client is used to interact with features provided by the alerting.caicloud.io group.
type AlertingV1alpha2Client struct {
	restClient rest.Interface
}

func (c *AlertingV1alpha2Client) AlertingRules() AlertingRuleInterface {
	return newAlertingRules(c)
}

func (c *AlertingV1alpha2Client) AlertingSubRules() AlertingSubRuleInterface {
	return newAlertingSubRules(c)
}

// NewForConfig creates a new AlertingV1alpha2Client for the given config.
func NewForConfig(c *rest.Config) (*AlertingV1alpha2Client, error) {
	config := *c
	if err := setConfigDefaults(&config); err != nil {
		return nil, err
	}
	client, err := rest.RESTClientFor(&config)
	if err != nil {
		return nil, err
	}
	return &AlertingV1alpha2Client{client}, nil
}

// NewForConfigOrDie creates a new AlertingV1alpha2Client for the given config and
// panics if there is an error in the config.
func NewForConfigOrDie(c *rest.Config) *AlertingV1alpha2Client {
	client, err := NewForConfig(c)
	if err != nil {
		panic(err)
	}
	return client
}

// New creates a new AlertingV1alpha2Client for the given RESTClient.
func New(c rest.Interface) *AlertingV1alpha2Client {
	return &AlertingV1alpha2Client{c}
}

func setConfigDefaults(config *rest.Config) error {
	gv := v1alpha2.SchemeGroupVersion
	config.GroupVersion = &gv
	config.APIPath = "/apis"
	config.NegotiatedSerializer = serializer.DirectCodecFactory{CodecFactory: scheme.Codecs}

	if config.UserAgent == "" {
		config.UserAgent = rest.DefaultKubernetesUserAgent()
	}

	return nil
}

// RESTClient returns a RESTClient that is used to communicate
// with API server by this client implementation.
func (c *AlertingV1alpha2Client) RESTClient() rest.Interface {
	if c == nil {
		return nil
	}
	return c.restClient
}
