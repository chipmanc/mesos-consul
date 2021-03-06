package mesos

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/CiscoCloud/mesos-consul/config"
	"github.com/CiscoCloud/mesos-consul/consul"
	"github.com/CiscoCloud/mesos-consul/registry"
	"github.com/CiscoCloud/mesos-consul/state"

	consulapi "github.com/hashicorp/consul/api"
	proto "github.com/mesos/mesos-go/mesosproto"
	log "github.com/sirupsen/logrus"
)

type CacheEntry struct {
	service      *consulapi.AgentServiceRegistration
	isRegistered bool
}

type Mesos struct {
	Registry registry.Registry
	Agents   map[string]string
	Lock     sync.Mutex

	Leader    *proto.MasterInfo
	Masters   []*proto.MasterInfo
	started   sync.Once
	startChan chan struct{}

	IpOrder        []string
	WhiteList      string
	whitelistRegex *regexp.Regexp
	BlackList      string
	blacklistRegex *regexp.Regexp
	taskTag        map[string][]string

	Separator string

	ServiceName string
	ServiceTags []string
}

func New(c *config.Config) *Mesos {
	m := new(Mesos)

	if c.Zk == "" {
		return nil
	}
	m.Separator = c.Separator

	if len(c.WhiteList) > 0 {
		m.WhiteList = strings.Join(c.WhiteList, "|")
		log.WithField("whitelist", m.WhiteList).Debug("Using whitelist regex")
		re, err := regexp.Compile(m.WhiteList)
		if err != nil {
			// For now, exit if the regex fails to compile. If we read regexes from Consul
			// maybe we emit a warning and use the old regex
			//
			log.WithField("whitelist", m.WhiteList).Fatal("WhiteList regex failed to compile")
		}
		m.whitelistRegex = re
	} else {
		m.whitelistRegex = nil
	}

	if len(c.BlackList) > 0 {
		m.BlackList = strings.Join(c.BlackList, "|")
		log.WithField("blacklist", m.BlackList).Debug("Using blacklist regex")
		re, err := regexp.Compile(m.BlackList)
		if err != nil {
			// For now, exit if the regex fails to compile. If we read regexes from Consul
			// maybe we emit a warning and use the old regex
			//
			log.WithField("blacklist", m.BlackList).Fatal("BlackList regex failed to compile")
		}
		m.blacklistRegex = re
	} else {
		m.blacklistRegex = nil
	}

	var err error
	m.taskTag, err = buildTaskTag(c.TaskTag)
	if err != nil {
		log.WithField("task-tag", c.TaskTag).Fatal(err.Error())
	}

	m.ServiceName = cleanName(c.ServiceName, c.Separator)

	m.Registry = consul.New()

	if m.Registry == nil {
		log.Fatal("No registry specified")
	}

	m.zkDetector(c.Zk)

	m.IpOrder = strings.Split(c.MesosIpOrder, ",")
	for _, src := range m.IpOrder {
		switch src {
		case "netinfo", "host", "docker", "mesos":
		default:
			log.Fatalf("Invalid IP Search Order: '%v'", src)
		}
	}
	log.Debugf("m.IpOrder = '%v'", m.IpOrder)

	if c.ServiceTags != "" {
		m.ServiceTags = strings.Split(c.ServiceTags, ",")
	}

	return m
}

// buildTaskTag takes a slice of task-tag arguments from the command line
// and returns a map of tasks name patterns to slice of tags that should be applied.
func buildTaskTag(taskTag []string) (map[string][]string, error) {
	result := make(map[string][]string)

	for _, tt := range taskTag {
		parts := strings.Split(tt, ":")
		if len(parts) != 2 {
			return nil, errors.New("task-tag pattern invalid, must include 1 colon separator")
		}

		taskName := strings.ToLower(parts[0])
		log.WithField("task-tag", taskName).Debug("Using task-tag pattern")
		tags := strings.Split(parts[1], ",")

		if _, ok := result[taskName]; !ok {
			result[taskName] = tags
		} else {
			result[taskName] = append(result[taskName], tags...)
		}
	}

	return result, nil
}

func (m *Mesos) Refresh() error {
	sj, err := m.loadState()
	if err != nil {
		log.Warn("loadState failed: ", err.Error())
		return err
	}

	if sj.Leader == "" {
		return errors.New("Empty master")
	}

	if m.Registry.CacheCreate() {
		m.LoadCache()
	}

	m.parseState(sj)

	return nil
}

func (m *Mesos) loadState() (state.State, error) {
	var err error
	var sj state.State

	log.Debug("loadState() called")

	defer func() {
		if rec := recover(); rec != nil {
			err = errors.New("can't connect to Mesos")
		}
	}()

	mh := m.getLeader()
	if mh.Ip == "" {
		log.Warn("No master in zookeeper")
		return sj, errors.New("No master in zookeeper")
	}

	log.Infof("Zookeeper leader: %s:%s", mh.Ip, mh.PortString)

	log.Info("reloading from master ", mh.Ip)
	sj, err = m.loadFromMaster(mh.Ip, mh.PortString)

	if rip := leaderIP(sj.Leader); rip != mh.Ip {
		log.Warn("master changed to ", rip)
		sj, err = m.loadFromMaster(rip, mh.PortString)
	}

	return sj, err
}

func (m *Mesos) loadFromMaster(ip string, port string) (sj state.State, err error) {
	url := "http://" + ip + ":" + port + "/master/state.json"

	req, err := http.NewRequest("GET", url, nil)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return
	}

	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}

	err = json.Unmarshal(body, &sj)
	if err != nil {
		return
	}

	return sj, nil
}

func (m *Mesos) parseState(sj state.State) {
	log.Info("Running parseState")

	m.RegisterHosts(sj)
	log.Debug("Done running RegisterHosts")

	for _, fw := range sj.Frameworks {
		for _, task := range fw.Tasks {
			agent, ok := m.Agents[task.SlaveID]
			if ok && task.State == "TASK_RUNNING" {
				task.SlaveIP = agent
				m.registerTask(&task, agent)
			}
		}
	}

	m.Registry.Deregister()
}
