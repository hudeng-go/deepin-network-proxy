package DBus

import (
	"errors"
	com "github.com/DeepinProxy/Com"
	"os"
	"sync"

	config "github.com/DeepinProxy/Config"
	define "github.com/DeepinProxy/Define"
	route "github.com/DeepinProxy/IpRoute"
	newCGroups "github.com/DeepinProxy/NewCGroups"
	newIptables "github.com/DeepinProxy/NewIptables"
	netlink "github.com/linuxdeepin/go-dbus-factory/com.deepin.system.procs"
	"pkg.deepin.io/lib/dbusutil"
)

// manage all proxy handler
type Manager struct {

	// dbus
	procsService *netlink.Procs
	sesService   *dbusutil.Service
	sysService   *dbusutil.Service
	sigLoop      *dbusutil.SignalLoop

	// proxy handler
	handler []BaseProxy

	// cgroup manager
	mainController *newCGroups.Controller
	controllerMgr  *newCGroups.Manager

	// config
	config *config.ProxyConfig

	// iptables manager
	mainChain   *newIptables.Chain // main attach chain
	iptablesMgr *newIptables.Manager

	// route manager
	mainRoute *route.Route
	routeMgr  *route.Manager

	// if current listening
	runOnce *sync.Once
}

// make manager
func NewManager() *Manager {
	manager := &Manager{

	}
	return manager
}

// inti manager
func (m *Manager) Init() error {
	// init session dbus service to export service
	sesService, err := dbusutil.NewSystemService()
	if err != nil {
		logger.Warningf("init dbus session service failed, err:  %v", err)
		return err
	}
	m.sesService = sesService

	// init system dbus service to monitor service
	service, err := dbusutil.NewSystemService()
	if err != nil {
		logger.Warningf("init dbus system service failed, err:  %v", err)
		return err
	}
	// store service
	m.sysService = service
	// attach dbus object
	m.procsService = netlink.NewProcs(service.Conn())
	m.sigLoop = dbusutil.NewSignalLoop(service.Conn(), 10)
	m.controllerMgr = newCGroups.NewManager()
	return nil
}

// load config
func (m *Manager) LoadConfig() error {
	// get effective user config dir
	path, err := com.GetUserConfigDir()
	if err != nil {
		logger.Warningf("failed to get user home dir, user:%v, err: %v", os.Geteuid(), err)
		return err
	}
	// config
	m.config = config.NewProxyCfg()
	err = m.config.LoadPxyCfg(path)
	if err != nil {
		logger.Warningf("load config failed, path: %s, err: %v", path, err)
		return err
	}
	return nil
}

// write config
func (m *Manager) WriteConfig() error {
	// get config path
	path, err := com.GetUserConfigDir()
	if err != nil {
		logger.Warningf("[manager] get user home dir failed, user:%v, err: %v", os.Geteuid(), err)
		return err
	}
	err = m.config.WritePxyCfg(path)
	if err != nil {
		logger.Warningf("[manager] write config file failed, err: %v", err)
		return err
	}
	return nil
}

// create handler and export service
func (m *Manager) Export() error {
	// app
	appProxy := newProxy(define.App)
	// save manager
	appProxy.saveManager(m)
	// load config
	appProxy.loadConfig()
	// create cgroups controller
	err := appProxy.export(m.sesService)
	if err != nil {
		logger.Warningf("create app proxy controller failed, err: %v", err)
		return err
	}
	m.handler = append(m.handler, appProxy)

	// global
	globalProxy := newProxy(define.Global)
	// save manager
	globalProxy.saveManager(m)
	// load config
	globalProxy.loadConfig()
	// create cgroups controller
	err = globalProxy.export(m.sesService)
	if err != nil {
		logger.Warningf("export app proxy failed, err: %v", err)
		return err
	}
	m.handler = append(m.handler, globalProxy)

	// request dbus service
	err = m.sesService.RequestName(BusServiceName)
	if err != nil {
		logger.Warningf("request service name failed, err: %v", err)
		return err
	}
	return nil
}

func (m *Manager) Wait() {
	m.sesService.Wait()
}

// only run once method
func (m *Manager) Start() {
	// if need reset once
	if m.runOnce == nil {
		m.runOnce = new(sync.Once)
	}
	m.runOnce.Do(func() {
		// init cgroups
		_ = m.initCGroups()

		// iptables init
		_ = m.initIptables()

		// init route
		_ = m.initRoute()

		err := m.Listen()
		if err != nil {
			logger.Warning("init iptables failed, err: %v", err)
		}
	})
}

// init iptables
func (m *Manager) initIptables() error {
	var err error
	m.iptablesMgr = newIptables.NewManager()
	m.iptablesMgr.Init()
	// get mangle output chain
	outputChain := m.iptablesMgr.GetChain("mangle", "OUTPUT")
	// create main chain to manager all children chain
	// sudo iptables -t mangle -N Main
	// sudo iptables -t mangle -A OUTPUT -j main
	m.mainChain, err = outputChain.CreateChild(define.Main.ToString(), 0, &newIptables.CompleteRule{Action: define.Main.ToString()})
	if err != nil {
		logger.Warningf("init iptables failed, err: %v", err)
		return err
	}
	// mainChain add default rule
	// iptables -t mangle -A All_Entry -m cgroup --path main.slice -j RETURN
	extends := newIptables.ExtendsRule{
		// -m
		Match: "m",
		// cgroup --path main.slice
		Elem: newIptables.ExtendsElem{
			// cgroup
			Match: "cgroup",
			// --path main.slice
			Base: newIptables.BaseRule{
				Match: "path", Param: define.Main.ToString() + ".slice",
			},
		},
	}
	// one complete rule
	cpl := &newIptables.CompleteRule{
		// -j RETURN
		Action: newIptables.RETURN,
		BaseSl: nil,
		// -m cgroup --path main.slice -j RETURN
		ExtendsSl: []newIptables.ExtendsRule{extends},
	}
	// append rule
	err = m.mainChain.AppendRule(cpl)
	if err != nil {
		logger.Warningf("init iptables failed, err: %v", err)
		return err
	}
	logger.Debug("init iptables success")
	return err
}

// init cgroups
func (m *Manager) initCGroups() error {
	// create controller
	var err error
	m.mainController, err = m.controllerMgr.CreatePriorityController(define.Main, define.MainPriority)
	if err != nil {
		logger.Warningf("init cgroup failed, err: %v", err)
		return err
	}
	logger.Debug("init cgroup success")
	return nil
}

// init route
func (m *Manager) initRoute() error {
	var err error
	m.routeMgr = route.NewManager()
	node := route.RouteNodeSpec{
		Type:   "local",
		Prefix: "default",
	}
	info := route.RouteInfoSpec{
		Dev: "lo",
	}
	m.mainRoute, err = m.routeMgr.CreateRoute("100", node, info)
	if err != nil {
		logger.Warningf("init route failed, err: %v", err)
		return err
	}
	logger.Debug("init route success")
	return nil
}

// format current procs
func (m *Manager) GetAllProcs() (map[string]newCGroups.ControlProcSl, error) {
	// check service
	if m.procsService == nil {
		logger.Warning("[manager] get procs failed, service not init")
		return nil, errors.New("service not init")
	}
	// get procs message
	// map[pid]{pid exec cgroups}
	procs, err := m.procsService.Procs().Get(0)
	if err != nil {
		logger.Warningf("[%s] get procs failed, err: %v", err)
		return nil, err
	}
	// map[exec][pid exec cgroups]
	ctrlProcMap := make(map[string]newCGroups.ControlProcSl)
	for _, proc := range procs {
		execPath := proc.ExecPath
		ctrlProcSl, ok := ctrlProcMap[execPath]
		// if not exist, add one
		if !ok {
			ctrlProcSl = newCGroups.ControlProcSl{}
			ctrlProcMap[execPath] = ctrlProcSl
		}
		// append
		ctrlProcSl = append(ctrlProcSl, &proc)
	}
	return ctrlProcMap, nil
}

// start listen
func (m *Manager) Listen() error {
	m.procsService.InitSignalExt(m.sigLoop, true)
	_, err := m.procsService.ConnectExecProc(func(execPath string, cwdPath string, pid string) {
		// search controller according to exe path, get highest priority one
		controller := m.controllerMgr.GetControllerByCtlPath(execPath)
		if controller == nil {
			return
		}
		proc := &netlink.ProcMessage{
			ExecPath: execPath,
			Pid:      pid,
		}
		// add to cgroups.procs and save
		err := controller.AddCtrlProc(proc)
		if err != nil {
			logger.Warningf("[%s] add exec %s to cgroups failed, err: %v", controller.Name, execPath, err)
		}
	})
	if err != nil {
		logger.Warningf("connect exec proc failed, err: %v")
		return err
	}
	_, err = m.procsService.ConnectExitProc(func(execPath string, cwdPath string, pid string) {
		// search controller according to exe path
		controller := m.controllerMgr.GetControllerByCtlPath(execPath)
		if controller == nil {
			return
		}
		proc := &netlink.ProcMessage{
			ExecPath: execPath,
			Pid:      pid,
		}
		// del from save
		err := controller.DelCtlProc(proc)
		if err != nil {
			logger.Warningf("[%s] del exec %s from cgroups failed, err: %v", controller.Name, execPath, err)
		}
	})
	if err != nil {
		logger.Warningf("connect exit proc failed, err: %v")
		return err
	}
	m.sigLoop.Start()
	return nil
}

// release all source
func (m *Manager) release() error {
	// check if all app and global proxy has stopped
	if m.mainChain.GetChildrenCount() != 0 {
		return nil
	}
	// remove all handler
	m.procsService.RemoveAllHandlers()
	// stop loop
	m.sigLoop.Stop()

	// remove chain
	err := m.mainChain.Remove()
	if err != nil {
		logger.Warningf("[manager] remove main chain failed, err: %v", err)
		return err
	}
	m.iptablesMgr = nil

	// release all control procs
	err = m.mainController.ReleaseAll()
	if err != nil {
		logger.Warning("[manager] release all control procs failed, err:", err)
		return err
	}
	m.controllerMgr = nil

	// remove all route
	err = m.mainRoute.Remove()
	if err != nil {
		logger.Warning("[manager] remove all route failed, err:", err)
		return err
	}
	m.routeMgr = nil

	// reset once
	m.runOnce = nil
	return nil
}
