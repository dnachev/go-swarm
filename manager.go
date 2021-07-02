package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"strings"
	"time"

	"github.com/prologic/jsonlines"
	log "github.com/sirupsen/logrus"

	"gitlab.mgt.aom.australiacloud.com.au/aom/golib/runcmd"
)

const (
	infoCommand        = `docker info --format "{{ json . }}"`
	nodesCommand       = `docker node ls --format "{{ json . }}"`
	tasksCommand       = `docker node ps --format "{{ json .}}" %s`
	initCommand        = `docker swarm init --advertise-addr %s --listen-addr %s`
	joinCommand        = `docker swarm join --advertise-addr %s --listen-addr %s --token %s %s:2377`
	tokenCommand       = `docker swarm join-token -q %s`
	updateCommand      = `docker node update %s %s`
	setAvailability    = `--availability %s`
	labelAdd           = `--label-add %s`
	availabilityDrain  = `drain`
	availabilityActive = `active`

	managerToken = "manager"
	workerToken  = "worker"

	drainTimeout = time.Minute * 10 // 10 minutes
)

// Manager manages all operations of a Docker Swarm cluster with flexible
// Switcher implementations that permit talking to Docker Nodes over different
// types of transport (e.g: local or remote).
type Manager struct {
	switcher Switcher
}

// NewManager constructs a new Manager type with the provider Switcher
func NewManager(switcher Switcher) (*Manager, error) {
	return &Manager{switcher: switcher}, nil
}

// Switcher returns the current Switcher for the manager being used
func (m *Manager) Switcher() Switcher {
	return m.switcher
}

// Runner returns the current Runner for the current Switcher being used
func (m *Manager) Runner() runcmd.Runner {
	return m.Switcher().Runner()
}

// SwitchNode switches to a new node given by nodeAddr to perform operations on
func (m *Manager) SwitchNode(nodeAddr string) error {
	if err := m.Switcher().Switch(nodeAddr); err != nil {
		log.WithError(err).Errorf("error switching to node %s", nodeAddr)
		return fmt.Errorf("error switching to node %s: %s", nodeAddr, err)
	}

	return nil
}

func (m *Manager) runCmd(cmd string, args ...string) (io.Reader, error) {
	if m.Runner() == nil {
		return nil, fmt.Errorf("error no runner configured")
	}

	log.WithField("args", args).Debugf("running cmd on %s: %s", m.switcher.String(), cmd)

	worker, err := m.Runner().Command(cmd)
	if err != nil {
		return nil, fmt.Errorf("error creating worker: %w", err)
	}

	stdout := &bytes.Buffer{}
	worker.SetStdout(stdout)

	stderr := &bytes.Buffer{}
	worker.SetStderr(stderr)

	if err := worker.Start(); err != nil {
		return nil, fmt.Errorf("error starting worker: %w", err)
	}

	if err := worker.Wait(); err != nil {
		log.WithError(err).
			WithField("stdout", string(stdout.String())).
			WithField("stderr", string(stderr.String())).
			Error("error running worker")
		return nil, fmt.Errorf("error running worker: %s", err)
	}

	return stdout, nil
}

func (m *Manager) ensureManager() error {
	node, err := m.GetInfo()
	if err != nil {
		return fmt.Errorf("error getting node info: %w", err)
	}
	if !node.IsManager() {
		for _, remoteManager := range node.Swarm.RemoteManagers {
			host, _, err := net.SplitHostPort(remoteManager.Addr)
			if err != nil {
				log.WithError(err).Warn("error parsing remote manager address (trying next manager): %w", err)
				continue
			}
			if err := m.SwitchNode(host); err != nil {
				log.WithError(err).Warn("error switch to remote manager (trying next manager): %w", err)
				continue
			}
			return nil
		}
		return fmt.Errorf("unable to connect to suitable manager")
	}

	return nil
}

func (m *Manager) joinSwarm(newNode VMNode, managerNode VMNode, token string) error {
	if err := m.SwitchNode(newNode.PublicAddress); err != nil {
		return fmt.Errorf("error switching nodes to %s: %w", newNode.PublicAddress, err)
	}

	cmd := fmt.Sprintf(
		joinCommand,
		newNode.PrivateAddress,
		newNode.PrivateAddress,
		token,
		managerNode.PrivateAddress,
	)
	_, err := m.runCmd(cmd)
	if err != nil {
		return fmt.Errorf("error running join command: %w", err)
	}

	return nil
}

func (m *Manager) labelNode(node VMNode) error {
	if err := m.SwitchNode(node.PublicAddress); err != nil {
		return fmt.Errorf("error switching nodes to %s: %w", node.PublicAddress, err)
	}

	info, err := m.GetInfo()
	if err != nil {
		return fmt.Errorf("error getting node info from: %w", err)
	}

	labelOptions := []string{}

	labels, err := ParseLabels(node.GetTag(LabelsTag))
	if err != nil {
		log.WithError(err).Error("error parsing labels")
		return fmt.Errorf("error parsing labels: %w", err)
	}

	if labels == nil || len(labels) == 0 {
		// No labels, nothing to do.
		return nil
	}

	for key, values := range labels {
		label := key
		if values != nil || len(values) > 0 {
			label += fmt.Sprintf("=%s", strings.Join(values, ","))
		}
		labelOptions = append(labelOptions, fmt.Sprintf(labelAdd, label))
	}

	cmd := fmt.Sprintf(
		updateCommand,
		strings.Join(labelOptions, " "),
		info.Swarm.NodeID,
	)
	_, err = m.runCmd(cmd)
	if err != nil {
		return fmt.Errorf("error running update command: %w", err)
	}

	return nil
}

// GetInfo returns information about the current node
func (m *Manager) GetInfo() (NodeInfo, error) {
	var node NodeInfo

	cmd := infoCommand
	out, err := m.runCmd(cmd)
	if err != nil {
		return NodeInfo{}, fmt.Errorf("error running info command: %w", err)
	}

	data, err := ioutil.ReadAll(out)
	if err != nil {
		return NodeInfo{}, fmt.Errorf("error reading info command output: %w", err)
	}

	if err := json.Unmarshal(data, &node); err != nil {
		return NodeInfo{}, fmt.Errorf("error parsing json data: %s", err)
	}

	return node, nil
}

// GetManagers returns a list of manager nodes and their information
func (m *Manager) GetManagers() ([]NodeInfo, error) {
	node, err := m.GetInfo()
	if err != nil {
		return nil, fmt.Errorf("error getting node info: %w", err)
	}

	var managers []NodeInfo
	for _, remoteManager := range node.Swarm.RemoteManagers {
		host, _, err := net.SplitHostPort(remoteManager.Addr)
		if err != nil {
			return nil, fmt.Errorf("error parsing remote manager address: %w", err)
		}
		if err := m.SwitchNode(host); err != nil {
			return nil, fmt.Errorf("error switching nodes to %s: %w", host, err)
		}
		node, err := m.GetInfo()
		if err != nil {
			return nil, fmt.Errorf("error getting manager node info: %w", err)
		}
		managers = append(managers, node)
	}

	return managers, nil
}

// GetNodes returns all nodes in the cluster
func (m *Manager) GetNodes() ([]NodeStatus, error) {
	if err := m.ensureManager(); err != nil {
		return nil, fmt.Errorf("error connecting to manager node: %w", err)
	}

	cmd := nodesCommand
	stdout, err := m.runCmd(cmd)
	if err != nil {
		return nil, fmt.Errorf("error running nodes command: %w", err)
	}

	var nodes []NodeStatus

	if err := jsonlines.Decode(stdout, &nodes); err != nil {
		return nil, fmt.Errorf("error parsing json data: %s", err)
	}

	return nodes, nil
}

// CreateSwarm creates a new Docker Swarm cluster given a set of nodes
func (m *Manager) CreateSwarm(vms VMNodes) error {
	managers := vms.FilterByTag(RoleTag, ManagerRole)
	if !(len(managers) == 3 || len(managers) == 5) {
		return fmt.Errorf("error expected 3 or 5 managers but got %d", len(managers))
	}

	workers := vms.FilterByTag(RoleTag, WorkerRole)

	// Pick a random manager out of the candidates
	randomIndex := rand.Intn(len(managers))
	manager := managers[randomIndex]

	if err := m.SwitchNode(manager.PublicAddress); err != nil {
		return fmt.Errorf("error switching to a manager node: %w", err)
	}

	node, err := m.GetInfo()
	if err != nil {
		return fmt.Errorf("error getting node info: %w", err)
	}

	clusterID := node.Swarm.Cluster.ID

	if clusterID != "" {
		return fmt.Errorf("error swarm cluster with id %s already exists", clusterID)
	}

	cmd := fmt.Sprintf(initCommand, manager.PrivateAddress, manager.PrivateAddress)
	if _, err := m.runCmd(cmd); err != nil {
		return fmt.Errorf("error running init command: %w", err)
	}
	if err := m.labelNode(manager); err != nil {
		return fmt.Errorf("error labelling worker: %w", err)
	}

	// Refresh node and get new Swarm Clsuter ID
	node, err = m.GetInfo()
	if err != nil {
		return fmt.Errorf("error refreshing node info: %w", err)
	}
	clusterID = node.Swarm.Cluster.ID

	managerToken, err := m.JoinToken(managerToken)
	if err != nil {
		return fmt.Errorf("error getting manager join token: %w", err)
	}

	workerToken, err := m.JoinToken(workerToken)
	if err != nil {
		return fmt.Errorf("error getting worker join token: %w", err)
	}

	// Join remaining managers
	for _, newManager := range managers {
		// Skip the leader we just created the swarm with
		if newManager.PublicAddress == manager.PublicAddress {
			continue
		}

		if err := m.joinSwarm(newManager, manager, managerToken); err != nil {
			return fmt.Errorf(
				"error joining manager %s to %s on swarm clsuter %s: %w",
				newManager.PublicAddress, manager.PublicAddress,
				clusterID, err,
			)
		}
		if err := m.labelNode(newManager); err != nil {
			return fmt.Errorf("error labelling manager: %w", err)
		}
	}

	// Join workers
	for _, worker := range workers {
		if err := m.joinSwarm(worker, manager, workerToken); err != nil {
			return fmt.Errorf(
				"error joining worker %s to %s on swarm clsuter %s: %w",
				worker.PublicAddress, manager.PublicAddress,
				clusterID, err,
			)
		}
		if err := m.labelNode(worker); err != nil {
			return fmt.Errorf("error labelling worker: %w", err)
		}
	}

	if err := m.SwitchNode(manager.PublicAddress); err != nil {
		return fmt.Errorf("error switching to manager node: %w", err)
	}

	return nil
}

// UpdateSwarm updates an existing Docker Swarm cluster by adding any
// missing manager or worker nodes that aren't already part of the cluster
func (m *Manager) UpdateSwarm(vms VMNodes) error {
	currentNodes := make(map[string]bool)

	nodes, err := m.GetNodes()
	if err != nil {
		return fmt.Errorf("error getting current nodes: %w", err)
	}
	for _, node := range nodes {
		currentNodes[node.Hostname] = true
	}

	var newNodes VMNodes

	for _, vm := range vms {
		if _, ok := currentNodes[vm.Hostname]; !ok {
			newNodes = append(newNodes, vm)
		}
	}

	managers := vms.FilterByTag(RoleTag, ManagerRole)
	if !(len(managers) == 3 || len(managers) == 5) {
		return fmt.Errorf("error expected 3 or 5 managers but got %d", len(managers))
	}

	// Pick a random manager out of the candidates
	randomIndex := rand.Intn(len(managers))
	manager := managers[randomIndex]

	newWorkers := newNodes.FilterByTag(RoleTag, WorkerRole)
	newManagers := newNodes.FilterByTag(RoleTag, ManagerRole)

	if err := m.ensureManager(); err != nil {
		return fmt.Errorf("error connecting to manager node: %w", err)
	}

	node, err := m.GetInfo()
	if err != nil {
		return fmt.Errorf("error getting node info: %w", err)
	}

	clusterID := node.Swarm.Cluster.ID

	if clusterID == "" {
		return fmt.Errorf("error no swarm cluster found")
	}

	managerToken, err := m.JoinToken(managerToken)
	if err != nil {
		return fmt.Errorf("error getting manager join token: %w", err)
	}

	workerToken, err := m.JoinToken(workerToken)
	if err != nil {
		return fmt.Errorf("error getting worker join token: %w", err)
	}

	// Join new managers
	for _, newManager := range newManagers {
		if err := m.joinSwarm(newManager, manager, managerToken); err != nil {
			return fmt.Errorf(
				"error joining manager %s to %s on swarm clsuter %s: %w",
				newManager.PublicAddress, manager.PublicAddress,
				clusterID, err,
			)
		}
		if err := m.labelNode(newManager); err != nil {
			return fmt.Errorf("error labelling manager: %w", err)
		}
	}

	// Join new workers
	for _, newWorker := range newWorkers {
		if err := m.joinSwarm(newWorker, manager, workerToken); err != nil {
			return fmt.Errorf(
				"error joining worker %s to %s on swarm clsuter %s: %w",
				newWorker.PublicAddress, manager.PublicAddress,
				clusterID, err,
			)
		}
		if err := m.labelNode(newWorker); err != nil {
			return fmt.Errorf("error labelling worker: %w", err)
		}
	}

	if err := m.SwitchNode(manager.PublicAddress); err != nil {
		return fmt.Errorf("error switching to manager node: %w", err)
	}

	return nil
}

func (m *Manager) getTasks(node string) (Tasks, error) {
	cmd := fmt.Sprintf(tasksCommand, node)
	stdout, err := m.runCmd(cmd)
	if err != nil {
		return nil, fmt.Errorf("error running tasks command: %w", err)
	}

	var tasks Tasks

	if err := jsonlines.Decode(stdout, &tasks); err != nil {
		return nil, fmt.Errorf("error parsing json data: %s", err)
	}

	return tasks, nil
}

func (m *Manager) drainNode(node string) error {
	startedAt := time.Now()

	cmd := fmt.Sprintf(updateCommand, fmt.Sprintf(setAvailability, availabilityDrain), node)
	_, err := m.runCmd(cmd)
	if err != nil {
		return fmt.Errorf("error running update command: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	ticker := time.NewTicker(time.Second * 5)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			elapsed := time.Now().Sub(startedAt)

			tasks, err := m.getTasks(node)
			if err != nil {
				log.WithError(err).Warnf("error getting tasks from node %s (retrying)", node)
				continue
			}

			if tasks.AllShutdown() {
				log.Infof("Successfully drained %s after %s", node, elapsed)
				return nil
			}

			log.Infof("Still waiting for %s to drain after %s ...", node, elapsed)
		case <-ctx.Done():
			elapsed := time.Now().Sub(startedAt)
			log.Errorf("timed out waiting for %s to drain after %s", node, elapsed)
			return fmt.Errorf("error timed out waiting for %s to drain after %s", node, elapsed)
		}
	}

	// Unreachable
}

// DrainNodes drains one or more nodes from an existing Docker Swarm cluster
// and blocks until there are no more tasks running on thoese nodes.
func (m *Manager) DrainNodes(nodes []string) error {
	if err := m.ensureManager(); err != nil {
		return fmt.Errorf("error connecting to manager node: %w", err)
	}

	for _, node := range nodes {
		if err := m.drainNode(node); err != nil {
			log.WithError(err).Errorf("error draining node: %s", node)
			return fmt.Errorf("error draining node %s: %w", node, err)
		}
	}

	return nil
}

// JoinToken retrieves the current join token for the given type
// "manager" or "worker" from any of the managers in the cluster
func (m *Manager) JoinToken(tokenType string) (string, error) {
	cmd := fmt.Sprintf(tokenCommand, tokenType)
	stdout, err := m.runCmd(cmd)
	if err != nil {
		return "", fmt.Errorf("error running token command: %w", err)
	}

	data, err := ioutil.ReadAll(stdout)
	if err != nil {
		return "", fmt.Errorf("error reading stdout: %w", err)
	}

	return strings.TrimSpace(string(data)), nil
}
