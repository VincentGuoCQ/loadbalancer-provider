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
	"io/ioutil"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/caicloud/loadbalancer-provider/pkg/execd"
	log "k8s.io/klog"
)

const (
	iptablesChain  = "LOADBALANCER-IPVS-DR"
	keepalivedCfg  = "/etc/keepalived/keepalived.conf"
	keepalivedTmpl = "/root/keepalived.tmpl"
	roleFilePath   = "/etc/keepalived/"
	roleMaster     = "master"
	checkInterval  = 5
	failedLimit    = 2

	acceptMark = 1
	dropMark   = 0
	mask       = "0x00000001"
)

type vrrpInstance struct {
	Name      string
	State     string
	Vrid      int
	Priority  int
	Interface string
	MyIP      string
	AllIPs    []string
	VIP       string
}

type virtualServer struct {
	AcceptMark int
	VIP        string
	Scheduler  string
	RealServer []string
}

type virtualInstanceMap struct {
	vis     map[string]*vrrpInstance
	visLock sync.RWMutex
}

func (m *virtualInstanceMap) Init() {
	m.vis = make(map[string]*vrrpInstance)
}

func (m *virtualInstanceMap) Set(key string, value *vrrpInstance) {
	m.visLock.Lock()
	defer m.visLock.Unlock()

	m.vis[key] = value
}

func (m *virtualInstanceMap) Get(key string) (*vrrpInstance, bool) {
	m.visLock.RLock()
	defer m.visLock.RUnlock()

	a, b := m.vis[key]
	return a, b
}

func (m *virtualInstanceMap) List() map[string]*vrrpInstance {
	m.visLock.RLock()
	defer m.visLock.RUnlock()

	return m.vis
}

type kaStatusCheck struct {
	ka        *keepalived
	stop      chan bool
	failCount int
	interval  int
	viMap     virtualInstanceMap
}

func (c *kaStatusCheck) updateVis(vis []*vrrpInstance) {
	for _, vi := range vis {
		c.viMap.Set(vi.Name, vi)
	}
}

func (c *kaStatusCheck) getInstanceRole(name string) (string, error) {
	filePath := roleFilePath + "role_" + name + ".conf"

	fp, err := os.Open(filePath)
	defer func(fp *os.File) {
		_ = fp.Close()
	}(fp)

	if err != nil {
		return "", err
	}
	buf, err := ioutil.ReadAll(fp)
	if err != nil {
		log.Errorf("read file %s err: %v", filePath, err)
		return "", err
	}

	role := strings.Replace(string(buf), "\n", "", -1)
	return role, err
}

func (c *kaStatusCheck) checkRDConfig() bool {
	roles := make(map[string]string)
	for _, vi := range c.viMap.List() {
		role, err := c.getInstanceRole(vi.Name)
		if err != nil {
			continue
		}
		roles[vi.Name] = role
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		log.Errorf("Failed to list ifaces error: %v", err)
		return true
	}

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			log.Errorf("Failed to list iface %s addr error: %v", iface.Name, err)
			return true
		}
		curIP := make(map[string]string)
		for _, addr := range addrs {
			add := strings.Split(addr.String(), "/")[0]
			curIP[add] = add
		}
		if (iface.Flags & net.FlagLoopback) != 0 {
			for _, vi := range c.viMap.List() {
				if _, ok := curIP[vi.VIP]; !ok {
					return false
				}
			}
		} else {
			for _, vi := range c.viMap.List() {
				if vi.Interface != iface.Name {
					continue
				}
				role, ok := roles[vi.Name]
				if !ok || role != roleMaster {
					continue
				}
				if _, ok := curIP[vi.VIP]; !ok {
					return false
				}
			}
		}
	}

	return true
}

func (c *kaStatusCheck) handleRDCheckRet(ok bool) {
	if ok {
		c.failCount = 0
		return
	}
	c.failCount++
	if c.failCount > failedLimit {
		log.Infof("keepalived config err, reload")
		err := c.ka.ForceRestart()
		if err != nil {
			log.Errorf("force restart keepalived error: %v", err)
		}
		c.failCount = 0
	}
}

func (c *kaStatusCheck) checkForever() {
	running := true
	timer := time.NewTimer(time.Duration(c.interval) * time.Second)
	log.Info("keepalived check start")
	for running {
		select {
		case <-timer.C:
			{
				ok := c.checkRDConfig()
				c.handleRDCheckRet(ok)
				timer.Reset(time.Duration(c.interval) * time.Second)
				break
			}
		case <-c.stop:
			{
				timer.Stop()
				running = false
				break
			}
		}
	}
	timer.Stop()
	log.Info("keepalived check stop")
}

func (c *kaStatusCheck) Start() {
	go c.checkForever()
}

func (c *kaStatusCheck) Stop() {
	c.stop <- true
}

type keepalived struct {
	cmd  *execd.D
	tmpl *template.Template
	stCk *kaStatusCheck
}

func (k *keepalived) UpdateConfig(vis []*vrrpInstance, vss []*virtualServer, httpPort int) error {
	w, err := os.Create(keepalivedCfg)
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()
	log.Infof("Updating keealived config")
	// save vips for release when shutting down

	conf := make(map[string]interface{})
	conf["iptablesChain"] = iptablesChain
	conf["instances"] = vis
	conf["vss"] = vss
	conf["httpPort"] = httpPort

	k.stCk.updateVis(vis)

	return k.tmpl.Execute(w, conf)
}

// Start starts a keepalived process in foreground.
// In case of any error it will terminate the execution with a fatal error
func (k *keepalived) Start() {
	go k.run()
	go k.stCk.Start()
}

func (k *keepalived) isRunning() bool {
	return k.cmd.IsRunning()
}

func (k *keepalived) run() {
	if err := k.cmd.RunForever(); err != nil {
		panic(fmt.Sprintf("can not run keepalived, %v", err))
	}
}

// Reload sends SIGHUP to keepalived to reload the configuration.
func (k *keepalived) Reload() error {
	log.Info("reloading keepalived")
	err := k.cmd.Signal(syscall.SIGHUP)
	if err == execd.ErrNotRunning {
		log.Warning("keepalived is not running, skip the reload")
		return nil
	}
	if err != nil {
		return fmt.Errorf("error reloading keepalived: %v", err)
	}

	return nil
}

func (k *keepalived) ForceRestart() error {
	log.Info("force restart keepalived")
	err := k.cmd.Signal(syscall.SIGTERM)
	if err == execd.ErrNotRunning {
		log.Warning("keepalived is not running, skip the force restart")
		return nil
	}
	if err != nil {
		return fmt.Errorf("error force restart keepalived: %v", err)
	}

	return nil
}

// Stop stop keepalived process
func (k *keepalived) Stop() {
	log.Info("stop keepalived process")
	err := k.cmd.Stop()
	if err != nil {
		log.Errorf("error stopping keepalived: %v", err)
	}
	k.stCk.Stop()
}

func (k *keepalived) Init() error {
	k.cmd = execd.Daemon("keepalived",
		"--dont-fork",
		"--log-console",
		"--release-vips",
		"--pid", "/keepalived.pid")
	// put keepalived in another process group to prevent it
	// to receive signals meant for the controller
	// k.cmd.SysProcAttr = &syscall.SysProcAttr{
	// 	Setpgid: true,
	// 	Pgid:    0,
	// }
	k.cmd.Stdout = os.Stdout
	k.cmd.Stderr = os.Stderr

	k.cmd.SetGracePeriod(1 * time.Second)
	tmpl, err := template.ParseFiles(keepalivedTmpl)
	if err != nil {
		return err
	}
	k.tmpl = tmpl

	k.stCk = &kaStatusCheck{
		ka:        k,
		stop:      make(chan bool, 1),
		failCount: 0,
		interval:  checkInterval,
	}
	k.stCk.viMap.Init()

	return nil
}
