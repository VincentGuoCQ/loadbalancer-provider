/*
Copyright 2020 caicloud authors. All rights reserved.
*/

// Code generated by lister-gen. DO NOT EDIT.

package v1alpha2

import (
	v1alpha2 "github.com/caicloud/clientset/pkg/apis/alerting/v1alpha2"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
)

// AlertingRuleLister helps list AlertingRules.
type AlertingRuleLister interface {
	// List lists all AlertingRules in the indexer.
	List(selector labels.Selector) (ret []*v1alpha2.AlertingRule, err error)
	// Get retrieves the AlertingRule from the index for a given name.
	Get(name string) (*v1alpha2.AlertingRule, error)
	AlertingRuleListerExpansion
}

// alertingRuleLister implements the AlertingRuleLister interface.
type alertingRuleLister struct {
	indexer cache.Indexer
}

// NewAlertingRuleLister returns a new AlertingRuleLister.
func NewAlertingRuleLister(indexer cache.Indexer) AlertingRuleLister {
	return &alertingRuleLister{indexer: indexer}
}

// List lists all AlertingRules in the indexer.
func (s *alertingRuleLister) List(selector labels.Selector) (ret []*v1alpha2.AlertingRule, err error) {
	err = cache.ListAll(s.indexer, selector, func(m interface{}) {
		ret = append(ret, m.(*v1alpha2.AlertingRule))
	})
	return ret, err
}

// Get retrieves the AlertingRule from the index for a given name.
func (s *alertingRuleLister) Get(name string) (*v1alpha2.AlertingRule, error) {
	obj, exists, err := s.indexer.GetByKey(name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.NewNotFound(v1alpha2.Resource("alertingrule"), name)
	}
	return obj.(*v1alpha2.AlertingRule), nil
}
