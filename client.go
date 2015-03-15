/*
Copyright (c) 2014 VMware, Inc. All Rights Reserved.

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

package govmomi

import (
	"errors"
	"net/url"

	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	"golang.org/x/net/context"
)

type Client struct {
	*soap.Client

	// RoundTripper is a separate field such that the client's implementation of
	// the RoundTripper interface can be wrapped by separate implementations for
	// extra functionality (for example, reauthentication on session timeout).
	RoundTripper soap.RoundTripper

	ServiceContent types.ServiceContent
	SessionManager *session.Manager
}

// NewClientFromClient creates and returns a new client structure from a
// soap.Client instance. The remote ServiceContent object is retrieved and
// populated in the Client structure before returning.
func NewClientFromClient(soapClient *soap.Client) (*Client, error) {
	serviceContent, err := methods.GetServiceContent(context.TODO(), soapClient)
	if err != nil {
		return nil, err
	}

	c := Client{
		Client:         soapClient,
		RoundTripper:   soapClient,
		ServiceContent: serviceContent,
	}

	c.SessionManager = session.NewManager(soapClient, c.ServiceContent)

	return &c, nil
}

// NewClient creates a new client from a URL. The client authenticates with the
// server before returning if the URL contains user information.
func NewClient(u *url.URL, insecure bool) (*Client, error) {
	soapClient := soap.NewClient(u, insecure)
	c, err := NewClientFromClient(soapClient)
	if err != nil {
		return nil, err
	}

	// Only login if the URL contains user information.
	if u.User != nil {
		err = c.SessionManager.Login(context.TODO(), u.User)
		if err != nil {
			return nil, err
		}
	}

	return c, nil
}

// convience method for logout via SessionManager
func (c *Client) Logout() error {
	err := c.SessionManager.Logout(context.TODO())

	// We've logged out - let's close any idle connections
	c.CloseIdleConnections()

	return err
}

// RoundTrip dispatches to the RoundTripper field.
func (c *Client) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	return c.RoundTripper.RoundTrip(ctx, req, res)
}

func (c *Client) Properties(obj types.ManagedObjectReference, p []string, dst interface{}) error {
	var objs = []types.ManagedObjectReference{obj}
	return c.PropertiesN(objs, p, dst)
}

func (c *Client) PropertiesN(objs []types.ManagedObjectReference, p []string, dst interface{}) error {
	var propSpec *types.PropertySpec
	var objectSet []types.ObjectSpec

	for _, obj := range objs {
		// Ensure that all object reference types are the same
		if propSpec == nil {
			propSpec = &types.PropertySpec{
				Type: obj.Type,
			}

			if p == nil {
				propSpec.All = true
			} else {
				propSpec.PathSet = p
			}
		} else {
			if obj.Type != propSpec.Type {
				return errors.New("object references must have the same type")
			}
		}

		objectSpec := types.ObjectSpec{
			Obj:  obj,
			Skip: false,
		}

		objectSet = append(objectSet, objectSpec)
	}

	req := types.RetrieveProperties{
		This: c.ServiceContent.PropertyCollector,
		SpecSet: []types.PropertyFilterSpec{
			{
				ObjectSet: objectSet,
				PropSet:   []types.PropertySpec{*propSpec},
			},
		},
	}

	return mo.RetrievePropertiesForRequest(context.TODO(), c, req, dst)
}

func (c *Client) WaitForProperties(obj types.ManagedObjectReference, ps []string, f func([]types.PropertyChange) bool) error {
	p, err := c.NewPropertyCollector()
	if err != nil {
		return err
	}

	defer p.Destroy()

	req := types.CreateFilter{
		Spec: types.PropertyFilterSpec{
			ObjectSet: []types.ObjectSpec{
				{
					Obj: obj,
				},
			},
			PropSet: []types.PropertySpec{
				{
					PathSet: ps,
					Type:    obj.Type,
				},
			},
		},
	}

	err = p.CreateFilter(req)
	if err != nil {
		return err
	}

	for version := ""; ; {
		res, err := p.WaitForUpdates(version)
		if err != nil {
			return err
		}

		version = res.Version

		for _, fs := range res.FilterSet {
			for _, os := range fs.ObjectSet {
				if os.Obj == obj {
					if f(os.ChangeSet) {
						return nil
					}
				}
			}
		}
	}
}

// Ancestors returns the entire ancestry tree of a specified managed object.
// The return value includes the root node and the specified object itself.
func (c *Client) Ancestors(obj types.ManagedObjectReference) ([]mo.ManagedEntity, error) {
	ospec := types.ObjectSpec{
		Obj: obj,
		SelectSet: []types.BaseSelectionSpec{
			&types.TraversalSpec{
				SelectionSpec: types.SelectionSpec{Name: "traverseParent"},
				Type:          "ManagedEntity",
				Path:          "parent",
				Skip:          false,
				SelectSet: []types.BaseSelectionSpec{
					&types.SelectionSpec{Name: "traverseParent"},
				},
			},
		},
		Skip: false,
	}

	pspec := types.PropertySpec{
		Type:    "ManagedEntity",
		PathSet: []string{"name", "parent"},
	}

	req := types.RetrieveProperties{
		This: c.ServiceContent.PropertyCollector,
		SpecSet: []types.PropertyFilterSpec{
			{
				ObjectSet: []types.ObjectSpec{ospec},
				PropSet:   []types.PropertySpec{pspec},
			},
		},
	}

	var ifaces []interface{}

	err := mo.RetrievePropertiesForRequest(context.TODO(), c, req, &ifaces)
	if err != nil {
		return nil, err
	}

	var out []mo.ManagedEntity

	// Build ancestry tree by iteratively finding a new child.
	for len(out) < len(ifaces) {
		var find types.ManagedObjectReference

		if len(out) > 0 {
			find = out[len(out)-1].Self
		}

		// Find entity we're looking for given the last entity in the current tree.
		for _, iface := range ifaces {
			me := iface.(mo.IsManagedEntity).GetManagedEntity()
			if me.Parent == nil {
				out = append(out, me)
				break
			}

			if *me.Parent == find {
				out = append(out, me)
				break
			}
		}
	}

	return out, nil
}

// NewPropertyCollector creates a new property collector based on the
// root property collector. It is the responsibility of the caller to
// clean up the property collector when done.
func (c *Client) NewPropertyCollector() (*PropertyCollector, error) {
	req := types.CreatePropertyCollector{
		This: c.ServiceContent.PropertyCollector,
	}

	res, err := methods.CreatePropertyCollector(context.TODO(), c, &req)
	if err != nil {
		return nil, err
	}

	p := PropertyCollector{
		c: c,
		r: res.Returnval,
	}

	return &p, nil
}
