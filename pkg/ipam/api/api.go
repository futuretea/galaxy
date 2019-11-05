/*
 * Tencent is pleased to support the open source community by making TKEStack available.
 *
 * Copyright (C) 2012-2019 Tencent. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not use
 * this file except in compliance with the License. You may obtain a copy of the
 * License at
 *
 * https://opensource.org/licenses/Apache-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
 * WARRANTIES OF ANY KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations under the License.
 */
package api

import (
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emicklei/go-restful"
	"k8s.io/client-go/listers/core/v1"
	glog "k8s.io/klog"
	"tkestack.io/galaxy/pkg/ipam/floatingip"
	"tkestack.io/galaxy/pkg/ipam/schedulerplugin/util"
	"tkestack.io/galaxy/pkg/utils/database"
	"tkestack.io/galaxy/pkg/utils/httputil"
	"tkestack.io/galaxy/pkg/utils/nets"
	pageutil "tkestack.io/galaxy/pkg/utils/page"
)

// Controller is the API controller
type Controller struct {
	ipam, secondIpam floatingip.IPAM
	podLister        v1.PodLister
}

// NewController construct a controller object
func NewController(ipam, secondIpam floatingip.IPAM, lister v1.PodLister) *Controller {
	return &Controller{
		ipam:       ipam,
		secondIpam: secondIpam,
		podLister:  lister,
	}
}

// FloatingIP is the floating ip info
type FloatingIP struct {
	IP        string `json:"ip"`
	Namespace string `json:"namespace,omitempty"`
	AppName   string `json:"appName,omitempty"`
	PodName   string `json:"podName,omitempty"`
	PoolName  string `json:"poolName,omitempty"`
	Policy    uint16 `json:"policy"`
	// Deprecate
	IsDeployment bool      `json:"isDeployment,omitempty"`
	AppType      string    `json:"appType,omitempty"`
	UpdateTime   time.Time `json:"updateTime,omitempty"`
	Status       string    `json:"status,omitempty"`
	Releasable   bool      `json:"releasable,omitempty"`
	attr         string    `json:"-"`
}

// SwaggerDoc is to generate Swagger docs
func (FloatingIP) SwaggerDoc() map[string]string {
	return map[string]string{
		"ip":           "ip",
		"namespace":    "namespace",
		"appName":      "deployment or statefulset name",
		"podName":      "pod name",
		"policy":       "ip release policy",
		"isDeployment": "deployment or statefulset, deprecated please set appType",
		"appType":      "deployment, statefulset or tapp",
		"updateTime":   "last allocate or release time of this ip",
		"status":       "pod status if exists",
		"releasable":   "if the ip is releasable. An ip is releasable if it isn't belong to any pod",
	}
}

// ListIPResp is the ListIPs response
type ListIPResp struct {
	pageutil.Page
	Content []FloatingIP `json:"content,omitempty"`
}

// ListIPs lists floating ips
func (c *Controller) ListIPs(req *restful.Request, resp *restful.Response) {
	keyword := req.QueryParameter("keyword")
	key := keyword
	fuzzyQuery := true
	if keyword == "" {
		fuzzyQuery = false
		poolName := req.QueryParameter("poolName")
		appName := req.QueryParameter("appName")
		podName := req.QueryParameter("podName")
		namespace := req.QueryParameter("namespace")
		appType := req.QueryParameter("appType")
		var appTypePrefix string
		if appType == "" {
			// compatible change with past api
			isDepStr := req.QueryParameter("isDeployment")
			if isDepStr != "" {
				isDep, err := strconv.ParseBool(isDepStr)
				if err != nil {
					httputil.BadRequest(resp, fmt.Errorf("invalid isDeployment(bool field): %s", isDepStr))
					return
				}
				if isDep {
					appTypePrefix = util.DeploymentPrefixKey
				} else {
					appTypePrefix = util.StatefulsetPrefixKey
				}
			} else {
				appTypePrefix = util.StatefulsetPrefixKey
			}
		} else {
			appTypePrefix = toAppTypePrefix(appType)
		}
		if appTypePrefix == "" {
			httputil.BadRequest(resp, fmt.Errorf("invalid appType %s", appType))
			return
		}
		key = util.NewKeyObj(appTypePrefix, namespace, appName, podName, poolName).KeyInDB
	}
	glog.V(4).Infof("list ips by %s, fuzzyQuery %v", key, fuzzyQuery)
	fips, err := listIPs(key, c.ipam, c.secondIpam, fuzzyQuery)
	if err != nil {
		httputil.InternalError(resp, err)
		return
	}
	sortParam, page, size := pageutil.PagingParams(req)
	sort.Sort(bySortParam{array: fips, lessFunc: sortFunc(sortParam)})
	start, end, pagin := pageutil.Pagination(page, size, len(fips))
	pagedFips := fips[start:end]
	if err := fillReleasableAndStatus(c.podLister, pagedFips); err != nil {
		httputil.InternalError(resp, err)
		return
	}
	resp.WriteEntity(ListIPResp{Page: *pagin, Content: pagedFips}) // nolint: errcheck
}

// toAppTypePrefix converts app name to app key prefix
func toAppTypePrefix(appType string) string {
	switch appType {
	case "deployment":
		return util.DeploymentPrefixKey
	case "statefulset", "statefulsets":
		return util.StatefulsetPrefixKey
	case "tapp":
		return util.TAppPrefixKey
	default:
		return ""
	}
}

// toAppType converts app key prefix to app name
func toAppType(appTypePrefix string) string {
	switch appTypePrefix {
	case util.DeploymentPrefixKey:
		return "deployment"
	case util.StatefulsetPrefixKey:
		return "statefulset"
	case util.TAppPrefixKey:
		return "tapp"
	default:
		return ""
	}
}

// fillReleasableAndStatus fills status and releasable field
func fillReleasableAndStatus(lister v1.PodLister, ips []FloatingIP) error {
	for i := range ips {
		ips[i].Releasable = true
		if ips[i].PodName == "" {
			continue
		}
		pod, err := lister.Pods(ips[i].Namespace).Get(ips[i].PodName)
		if err != nil || pod == nil {
			ips[i].Status = "Deleted"
			continue
		}
		ips[i].Status = string(pod.Status.Phase)
		// On public cloud, we can't release exist pod's ip, because we need to call unassign ip first
		// TODO while on private environment, we can
		ips[i].Releasable = false
	}
	return nil
}

// bySortParam defines sort funcs for FloatingIP array
type bySortParam struct {
	lessFunc func(a, b int, array []FloatingIP) bool
	array    []FloatingIP
}

func (by bySortParam) Less(a, b int) bool {
	return by.lessFunc(a, b, by.array)
}

func (by bySortParam) Swap(a, b int) {
	by.array[a], by.array[b] = by.array[b], by.array[a]
}

func (by bySortParam) Len() int {
	return len(by.array)
}

// sortFunc defines the sort algorithm
// #lizard forgives
func sortFunc(sort string) func(a, b int, array []FloatingIP) bool {
	switch strings.ToLower(sort) {
	case "namespace asc":
		return func(a, b int, array []FloatingIP) bool {
			return array[a].Namespace < array[b].Namespace
		}
	case "namespace desc":
		return func(a, b int, array []FloatingIP) bool {
			return array[a].Namespace > array[b].Namespace
		}
	case "podname":
		fallthrough
	case "podname asc":
		return func(a, b int, array []FloatingIP) bool {
			return array[a].PodName < array[b].PodName
		}
	case "podname desc":
		return func(a, b int, array []FloatingIP) bool {
			return array[a].PodName > array[b].PodName
		}
	case "policy":
		fallthrough
	case "policy asc":
		return func(a, b int, array []FloatingIP) bool {
			return array[a].Policy < array[b].Policy
		}
	case "policy desc":
		return func(a, b int, array []FloatingIP) bool {
			return array[a].Policy > array[b].Policy
		}
	case "ip desc":
		return func(a, b int, array []FloatingIP) bool {
			return array[a].IP > array[b].IP
		}
	case "ip":
		fallthrough
	case "ip asc":
		fallthrough
	default:
		return func(a, b int, array []FloatingIP) bool {
			return array[a].IP < array[b].IP
		}
	}
}

// ReleaseIPReq is the request to release ips
type ReleaseIPReq struct {
	IPs []FloatingIP `json:"ips"`
}

// ReleaseIPResp is the response of release ip
type ReleaseIPResp struct {
	httputil.Resp
	Unreleased []string `json:"unreleased,omitempty"`
}

// SwaggerDoc generates swagger doc for release ip response
func (ReleaseIPResp) SwaggerDoc() map[string]string {
	return map[string]string{
		"unreleased": "unreleased ips, have been released or allocated to other pods, or are not within valid range",
	}
}

// ReleaseIPs releases floating ips
// #lizard forgives
func (c *Controller) ReleaseIPs(req *restful.Request, resp *restful.Response) {
	var releaseIPReq ReleaseIPReq
	if err := req.ReadEntity(&releaseIPReq); err != nil {
		httputil.BadRequest(resp, err)
		return
	}
	expectIPtoKey := make(map[string]string)
	for i := range releaseIPReq.IPs {
		temp := releaseIPReq.IPs[i]
		ip := net.ParseIP(temp.IP)
		if ip == nil {
			httputil.BadRequest(resp, fmt.Errorf("%q is not a valid ip", temp.IP))
			return
		}
		var appTypePrefix string
		if temp.AppType == "" {
			if temp.IsDeployment {
				appTypePrefix = util.DeploymentPrefixKey
			} else {
				appTypePrefix = util.StatefulsetPrefixKey
			}
		} else {
			appTypePrefix = toAppTypePrefix(temp.AppType)
			if appTypePrefix == "" {
				httputil.BadRequest(resp, fmt.Errorf("unknown app type %q", temp.AppType))
				return
			}
		}
		keyObj := util.NewKeyObj(appTypePrefix, temp.Namespace, temp.AppName, temp.PodName, temp.PoolName)
		expectIPtoKey[temp.IP] = keyObj.KeyInDB
	}
	if err := fillReleasableAndStatus(c.podLister, releaseIPReq.IPs); err != nil {
		httputil.BadRequest(resp, err)
		return
	}
	for _, ip := range releaseIPReq.IPs {
		if !ip.Releasable {
			httputil.BadRequest(resp, fmt.Errorf("%s is not releasable", ip.IP))
			return
		}
	}
	_, unreleased, err := batchReleaseIPs(expectIPtoKey, c.ipam, c.secondIpam)
	var unreleasedIP []string
	for ip := range unreleased {
		unreleasedIP = append(unreleasedIP, ip)
	}
	var res *ReleaseIPResp
	if err != nil {
		res = &ReleaseIPResp{Resp: httputil.NewResp(
			http.StatusInternalServerError, fmt.Sprintf("server error: %v", err))}
	} else if len(unreleasedIP) > 0 {
		res = &ReleaseIPResp{Resp: httputil.NewResp(
			http.StatusAccepted, fmt.Sprintf("Unreleased ips have been released or allocated to other pods, "+
				"or are not within valid range"))}
	} else {
		res = &ReleaseIPResp{Resp: httputil.NewResp(http.StatusOK, "")}
	}
	res.Unreleased = unreleasedIP
	resp.WriteHeaderAndEntity(res.Code, res)
}

// listIPs lists ips from ipams
func listIPs(keyword string, ipam, secondIpam floatingip.IPAM, fuzzyQuery bool) ([]FloatingIP, error) {
	var fips []database.FloatingIP
	var err error
	if fuzzyQuery {
		fips, err = ipam.ByKeyword(keyword)
	} else {
		fips, err = ipam.ByPrefix(keyword)
	}
	if err != nil {
		return nil, err
	}
	resp := transform(fips)
	if secondIpam != nil {
		var secondFips []database.FloatingIP
		if fuzzyQuery {
			secondFips, err = secondIpam.ByKeyword(keyword)
		} else {
			secondFips, err = secondIpam.ByPrefix(keyword)
		}
		if err != nil {
			return resp, err
		}
		resp2 := transform(secondFips)
		resp = append(resp, resp2...)
	}
	return resp, nil
}

// transform converts `database.FloatingIP` slice to `FloatingIP` slice
func transform(fips []database.FloatingIP) []FloatingIP {
	var res []FloatingIP
	for i := range fips {
		keyObj := util.ParseKey(fips[i].Key)
		res = append(res, FloatingIP{IP: nets.IntToIP(fips[i].IP).String(),
			Namespace:    keyObj.Namespace,
			AppName:      keyObj.AppName,
			PodName:      keyObj.PodName,
			PoolName:     keyObj.PoolName,
			IsDeployment: keyObj.IsDeployment,
			AppType:      toAppType(keyObj.AppTypePrefix),
			Policy:       fips[i].Policy,
			UpdateTime:   fips[i].UpdatedAt,
			attr:         fips[i].Attr})
	}
	return res
}

// batchReleaseIPs release ips from ipams
func batchReleaseIPs(ipToKey map[string]string,
	ipam, secondIpam floatingip.IPAM) (map[string]string, map[string]string, error) {
	released, unreleased, err := ipam.ReleaseIPs(ipToKey)
	if len(released) > 0 {
		glog.Infof("releaseIPs %v", released)
	}
	if err != nil {
		return released, unreleased, err
	}
	if secondIpam != nil {
		released2, unreleased2, err := secondIpam.ReleaseIPs(unreleased)
		if len(released2) > 0 {
			glog.Infof("releaseIPs in second IPAM %v", released2)
		}
		for k, v := range released2 {
			released[k] = v
		}
		unreleased = unreleased2
		if err != nil {
			if !(strings.Contains(err.Error(), "Table") && strings.Contains(err.Error(), "doesn't exist")) {
				return released, unreleased, err
			}
		}
	}
	return released, unreleased, nil
}
