// Copyright 2016-2018 Authors of Cilium
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package endpoint

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cilium/cilium/api/v1/models"
	"github.com/cilium/cilium/common/addressing"
	"github.com/cilium/cilium/pkg/completion"
	"github.com/cilium/cilium/pkg/controller"
	identityPkg "github.com/cilium/cilium/pkg/identity"
	clientset "github.com/cilium/cilium/pkg/k8s/client/clientset/versioned"
	"github.com/cilium/cilium/pkg/labels"
	pkgLabels "github.com/cilium/cilium/pkg/labels"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/mac"
	"github.com/cilium/cilium/pkg/maps/policymap"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/policy"
	"github.com/cilium/cilium/pkg/proxy/accesslog"

	go_version "github.com/hashicorp/go-version"

	"github.com/sirupsen/logrus"
)

var (
	//IPv4Enabled can be set to false to indicate IPv6 only operation
	IPv4Enabled = true
)

// PortMap is the port mapping representation for a particular endpoint.
type PortMap struct {
	From  uint16 `json:"from"`
	To    uint16 `json:"to"`
	Proto uint8  `json:"proto"`
}

const (
	OptionConntrackAccounting = "ConntrackAccounting"
	OptionConntrackLocal      = "ConntrackLocal"
	OptionConntrack           = "Conntrack"
	OptionDebug               = "Debug"
	OptionDebugLB             = "DebugLB"
	OptionDropNotify          = "DropNotification"
	OptionTraceNotify         = "TraceNotification"
	OptionNAT46               = "NAT46"
	OptionIngressPolicy       = "IngressPolicy"
	OptionEgressPolicy        = "EgressPolicy"
	AlwaysEnforce             = "always"
	NeverEnforce              = "never"
	DefaultEnforcement        = "default"

	maxLogs = 256
)

var (
	OptionSpecConntrackAccounting = option.Option{
		Define:      "CONNTRACK_ACCOUNTING",
		Description: "Enable per flow (conntrack) statistics",
		Requires:    []string{OptionConntrack},
	}

	OptionSpecConntrackLocal = option.Option{
		Define:      "CONNTRACK_LOCAL",
		Description: "Use endpoint dedicated tracking table instead of global one",
		Requires:    []string{OptionConntrack},
	}

	OptionSpecConntrack = option.Option{
		Define:      "CONNTRACK",
		Description: "Enable stateful connection tracking",
	}

	OptionSpecDebug = option.Option{
		Define:      "DEBUG",
		Description: "Enable debugging trace statements",
	}

	OptionSpecDebugLB = option.Option{
		Define:      "LB_DEBUG",
		Description: "Enable debugging trace statements for load balancer",
	}

	OptionSpecDropNotify = option.Option{
		Define:      "DROP_NOTIFY",
		Description: "Enable drop notifications",
	}

	OptionSpecTraceNotify = option.Option{
		Define:      "TRACE_NOTIFY",
		Description: "Enable trace notifications",
	}

	OptionSpecNAT46 = option.Option{
		Define:      "ENABLE_NAT46",
		Description: "Enable automatic NAT46 translation",
		Requires:    []string{OptionConntrack},
		Verify: func(key string, val bool) error {
			if !IPv4Enabled {
				return fmt.Errorf("NAT46 requires IPv4 to be enabled")
			}
			return nil
		},
	}

	OptionIngressSpecPolicy = option.Option{
		Define:      "POLICY_INGRESS",
		Description: "Enable ingress policy enforcement",
	}

	OptionEgressSpecPolicy = option.Option{
		Define:      "POLICY_EGRESS",
		Description: "Enable egress policy enforcement",
	}

	EndpointMutableOptionLibrary = option.OptionLibrary{
		OptionConntrackAccounting: &OptionSpecConntrackAccounting,
		OptionConntrackLocal:      &OptionSpecConntrackLocal,
		OptionConntrack:           &OptionSpecConntrack,
		OptionDebug:               &OptionSpecDebug,
		OptionDebugLB:             &OptionSpecDebugLB,
		OptionDropNotify:          &OptionSpecDropNotify,
		OptionTraceNotify:         &OptionSpecTraceNotify,
		OptionNAT46:               &OptionSpecNAT46,
		OptionIngressPolicy:       &OptionIngressSpecPolicy,
		OptionEgressPolicy:        &OptionEgressSpecPolicy,
	}

	EndpointOptionLibrary = option.OptionLibrary{}

	// ciliumEPControllerLimit is the range of k8s versions with which we are
	// willing to run the EndpointCRD controllers
	ciliumEPControllerLimit, _ = go_version.NewConstraint("> 1.6")

	// ciliumEndpointSyncControllerK8sClient is a k8s client shared by the
	// RunK8sCiliumEndpointSync and RunK8sCiliumEndpointSyncGC. They obtain the
	// controller via getCiliumClient and the sync.Once is used to avoid race.
	ciliumEndpointSyncControllerOnce      sync.Once
	ciliumEndpointSyncControllerK8sClient clientset.Interface
)

func init() {
	for k, v := range EndpointMutableOptionLibrary {
		EndpointOptionLibrary[k] = v
	}
}

const (
	// ReservedCEPNamespace is the namespace to use for reserved endpoints that
	// don't have a namespace (e.g. health)
	ReservedCEPNamespace = meta_v1.NamespaceSystem

	// HealthCEPPrefix is the prefix used to name the cilium health endpoints' CEP
	HealthCEPPrefix = "cilium-health-"
)

// Endpoint represents a container or similar which can be individually
// addresses on L3 with its own IP addresses. This structured is managed by the
// endpoint manager in pkg/endpointmanager.
//
//
// WARNING - STABLE API
// This structure is written as JSON to StateDir/{ID}/lxc_config.h to allow to
// restore endpoints when the agent is being restarted. The restore operation
// will read the file and re-create all endpoints with all fields which are not
// marked as private to JSON marshal. Do NOT modify this structure in ways which
// is not JSON forward compatible.
//
type Endpoint struct {
	// ID of the endpoint, unique in the scope of the node
	ID uint16

	// Mutex protects write operations to this endpoint structure
	Mutex lock.RWMutex

	// ContainerName is the name given to the endpoint by the container runtime
	ContainerName string

	// DockerID is the container ID that containerd has assigned to the endpoint
	//
	// FIXME: Rename this field to ContainerID
	DockerID string

	// DockerNetworkID is the network ID of the libnetwork network if the
	// endpoint is a docker managed container which uses libnetwork
	DockerNetworkID string

	// DockerEndpointID is the Docker network endpoint ID if managed by
	// libnetwork
	DockerEndpointID string

	// IfName is the name of the host facing interface (veth pair) which
	// connects into the endpoint
	IfName string

	// IfIndex is the interface index of the host face interface (veth pair)
	IfIndex int

	// OpLabels is the endpoint's label configuration
	//
	// FIXME: Rename this field to Labels
	OpLabels pkgLabels.OpLabels

	// identityRevision is incremented each time the identity label
	// information of the endpoint has changed
	identityRevision int

	// LXCMAC is the MAC address of the endpoint
	//
	// FIXME: Rename this field to MAC
	LXCMAC mac.MAC // Container MAC address.

	// IPv6 is the IPv6 address of the endpoint
	IPv6 addressing.CiliumIPv6

	// IPv4 is the IPv4 address of the endpoint
	IPv4 addressing.CiliumIPv4

	// NodeMAC is the MAC of the node (agent). The MAC is different for every endpoint.
	NodeMAC mac.MAC

	// SecurityIdentity is the security identity of this endpoint. This is computed from
	// the endpoint's labels.
	SecurityIdentity *identityPkg.Identity `json:"SecLabel"`

	// LabelsHash is a SHA256 hash over the SecurityIdentity labels
	LabelsHash string

	// LabelsMap is the Set of all security labels used in the last policy computation
	LabelsMap *identityPkg.IdentityCache

	// PortMap is port mapping configuration of the endpoint
	PortMap []PortMap // Port mapping used for this endpoint.

	// Consumable represents the security-identity-based policy for this endpoint.
	Consumable *policy.Consumable `json:"-"`

	// L4Policy is the L4Policy in effect for the
	// endpoint. Outside of policy recalculation, it is the same as the
	// Consumable's L4Policy, but this is needed during policy recalculation to
	// be able to clean up PolicyMap after the endpoint's consumable has already
	// been updated.
	L4Policy *policy.L4Policy `json:"-"`

	// PolicyMap is the policy related state of the datapath including
	// reference to all policy related BPF
	PolicyMap *policymap.PolicyMap `json:"-"`

	// CIDRPolicy is the CIDR based policy configuration of the endpoint. This
	// is not contained within the Consumable for this endpoint because the
	// Consumable only contains identity-based policy information.
	L3Policy *policy.CIDRPolicy `json:"-"`

	// L3Maps is the datapath representation of CIDRPolicy
	L3Maps L3Maps `json:"-"`

	// Opts are configurable boolean options
	Opts *option.BoolOptions

	// Status are the last n state transitions this endpoint went through
	Status *EndpointStatus

	// state is the state the endpoint is in. See SetStateLocked()
	state string

	// PolicyCalculated is true as soon as the policy has been calculated
	// for the first time. As long as this value is false, all packets sent
	// by the endpoint will be dropped to ensure that the endpoint cannot
	// bypass policy while it is still being resolved.
	PolicyCalculated bool `json:"-"`

	k8sPodName   string
	k8sNamespace string

	// policyRevision is the policy revision this endpoint is currently on
	// to modify this field please use endpoint.setPolicyRevision instead
	policyRevision uint64

	// policyRevisionSignals contains a map of PolicyRevision signals that
	// should be triggered once the policyRevision reaches the wanted wantedRev.
	policyRevisionSignals map[policySignal]bool

	// proxyPolicyRevision is the policy revision that has been applied to
	// the proxy.
	proxyPolicyRevision uint64

	// proxyStatisticsMutex is the mutex that must be held to read or write
	// proxyStatistics.
	proxyStatisticsMutex lock.RWMutex

	// proxyStatistics contains statistics of proxy redirects.
	// They keys in this map are the ProxyStatistics with their
	// AllocatedProxyPort and Statistics fields set to 0 and nil.
	// You must hold Endpoint.proxyStatisticsMutex to read or write it.
	proxyStatistics map[models.ProxyStatistics]*models.ProxyStatistics

	// nextPolicyRevision is the policy revision that the endpoint has
	// updated to and that will become effective with the next regenerate
	nextPolicyRevision uint64

	// forcePolicyCompute full endpoint policy recomputation
	// Set when endpoint options have been changed. Cleared right before releasing the
	// endpoint mutex after policy recalculation.
	forcePolicyCompute bool

	// BuildMutex synchronizes builds of individual endpoints and locks out
	// deletion during builds
	//
	// FIXME: Mark private once endpoint deletion can be moved into
	// `pkg/endpoint`
	BuildMutex lock.Mutex

	// logger is a logrus object with fields set to report an endpoints information.
	// You must hold Endpoint.Mutex to read or write it (but not to log with it).
	logger *logrus.Entry

	// controllers is the list of async controllers syncing the endpoint to
	// other resources
	controllers controller.Manager

	// realizedRedirects maps the ID of each proxy redirect that has been
	// successfully added into a proxy for this endpoint, to the redirect's
	// proxy port number.
	// You must hold Endpoint.Mutex to read or write it.
	realizedRedirects map[string]uint16

	// ProxyWaitGroup waits for pending proxy changes to complete.
	// You must hold Endpoint.BuildMutex to read or write it.
	ProxyWaitGroup *completion.WaitGroup `json:"-"`
}

// WaitForProxyCompletions blocks until all proxy changes have been completed.
// Called with BuildMutex held.
func (e *Endpoint) WaitForProxyCompletions() error {
	start := time.Now()
	e.getLogger().Debug("Waiting for proxy updates to complete...")
	err := e.ProxyWaitGroup.Wait()
	if err != nil {
		return fmt.Errorf("proxy state changes failed: %s", err)
	}
	e.getLogger().Debug("Wait time for proxy updates: ", time.Since(start))
	return nil
}

// NewEndpointWithState creates a new endpoint useful for testing purposes
func NewEndpointWithState(ID uint16, state string) *Endpoint {
	return &Endpoint{
		ID:     ID,
		Opts:   option.NewBoolOptions(&EndpointOptionLibrary),
		Status: NewEndpointStatus(),
		state:  state,
	}
}

// GetID returns the endpoint's ID
func (e *Endpoint) GetID() uint64 {
	return uint64(e.ID)
}

// RLock locks the endpoint for reading
func (e *Endpoint) RLock() {
	e.Mutex.RLock()
}

// RUnlock unlocks the endpoint after reading
func (e *Endpoint) RUnlock() {
	e.Mutex.RUnlock()
}

// Lock locks the endpoint for reading  or writing
func (e *Endpoint) Lock() {
	e.Mutex.Lock()
}

// Unlock unlocks the endpoint after reading or writing
func (e *Endpoint) Unlock() {
	e.Mutex.Unlock()
}

// GetLabels returns the labels as slice
func (e *Endpoint) GetLabels() []string {
	if e.SecurityIdentity == nil {
		return []string{}
	}

	return e.SecurityIdentity.Labels.GetModel()
}

// GetLabelsSHA returns the SHA of labels
func (e *Endpoint) GetLabelsSHA() string {
	if e.SecurityIdentity == nil {
		return ""
	}

	return e.SecurityIdentity.GetLabelsSHA256()
}

// GetIPv4Address returns the IPv4 address of the endpoint
func (e *Endpoint) GetIPv4Address() string {
	return e.IPv4.String()
}

// GetIPv6Address returns the IPv6 address of the endpoint
func (e *Endpoint) GetIPv6Address() string {
	return e.IPv6.String()
}

// statusLogMsg represents a log message.
type statusLogMsg struct {
	Status    Status    `json:"status"`
	Timestamp time.Time `json:"timestamp"`
}

// statusLog represents a slice of statusLogMsg.
type statusLog []*statusLogMsg

// componentStatus represents a map of a single statusLogMsg by StatusType.
type componentStatus map[StatusType]*statusLogMsg

// contains checks if the given `s` statusLogMsg is present in the
// priorityStatus.
func (ps componentStatus) contains(s *statusLogMsg) bool {
	return ps[s.Status.Type] == s
}

// statusTypeSlice represents a slice of StatusType, is used for sorting
// purposes.
type statusTypeSlice []StatusType

// Len returns the length of the slice.
func (p statusTypeSlice) Len() int { return len(p) }

// Less returns true if the element `j` is less than element `i`.
// *It's reversed* so that we can sort the slice by high to lowest priority.
func (p statusTypeSlice) Less(i, j int) bool { return p[i] > p[j] }

// Swap swaps element in `i` with element in `j`.
func (p statusTypeSlice) Swap(i, j int) { p[i], p[j] = p[j], p[i] }

// sortByPriority returns a statusLog ordered from highest priority to lowest.
func (ps componentStatus) sortByPriority() statusLog {
	prs := statusTypeSlice{}
	for k := range ps {
		prs = append(prs, k)
	}
	sort.Sort(prs)
	slogSorted := statusLog{}
	for _, pr := range prs {
		slogSorted = append(slogSorted, ps[pr])
	}
	return slogSorted
}

// EndpointStatus represents the endpoint status.
type EndpointStatus struct {
	// CurrentStatuses is the last status of a given priority.
	CurrentStatuses componentStatus `json:"current-status,omitempty"`
	// Contains the last maxLogs messages for this endpoint.
	Log statusLog `json:"log,omitempty"`
	// Index is the index in the statusLog, is used to keep track the next
	// available position to write a new log message.
	Index int `json:"index"`
	// indexMU is the Mutex for the CurrentStatus and Log RW operations.
	indexMU lock.RWMutex
}

func NewEndpointStatus() *EndpointStatus {
	return &EndpointStatus{
		CurrentStatuses: componentStatus{},
		Log:             statusLog{},
	}
}

func (e *EndpointStatus) lastIndex() int {
	lastIndex := e.Index - 1
	if lastIndex < 0 {
		return maxLogs - 1
	}
	return lastIndex
}

// getAndIncIdx returns current free slot index and increments the index to the
// next index that can be overwritten.
func (e *EndpointStatus) getAndIncIdx() int {
	idx := e.Index
	e.Index++
	if e.Index >= maxLogs {
		e.Index = 0
	}
	// Lets skip the CurrentStatus message from the log to prevent removing
	// non-OK status!
	if e.Index < len(e.Log) &&
		e.CurrentStatuses.contains(e.Log[e.Index]) &&
		e.Log[e.Index].Status.Code != OK {
		e.Index++
		if e.Index >= maxLogs {
			e.Index = 0
		}
	}
	return idx
}

// addStatusLog adds statusLogMsg to endpoint log.
// example of e.Log's contents where maxLogs = 3 and Index = 0
// [index] - Priority - Code
// [0] - BPF - OK
// [1] - Policy - Failure
// [2] - BPF - OK
// With this log, the CurrentStatus will keep [1] for Policy priority and [2]
// for BPF priority.
//
// Whenever a new statusLogMsg is received, that log will be kept in the
// CurrentStatus map for the statusLogMsg's priority.
// The CurrentStatus map, ensures non of the failure messages are deleted for
// higher priority messages and vice versa.
func (e *EndpointStatus) addStatusLog(s *statusLogMsg) {
	e.CurrentStatuses[s.Status.Type] = s
	idx := e.getAndIncIdx()
	if len(e.Log) < maxLogs {
		e.Log = append(e.Log, s)
	} else {
		e.Log[idx] = s
	}
}

func (e *EndpointStatus) GetModel() []*models.EndpointStatusChange {
	e.indexMU.RLock()
	defer e.indexMU.RUnlock()

	list := []*models.EndpointStatusChange{}
	for i := e.lastIndex(); ; i-- {
		if i < 0 {
			i = maxLogs - 1
		}
		if i < len(e.Log) && e.Log[i] != nil {
			list = append(list, &models.EndpointStatusChange{
				Timestamp: e.Log[i].Timestamp.Format(time.RFC3339),
				Code:      e.Log[i].Status.Code.String(),
				Message:   e.Log[i].Status.Msg,
				State:     models.EndpointState(e.Log[i].Status.State),
			})
		}
		if i == e.Index {
			break
		}
	}
	return list
}

func (e *EndpointStatus) CurrentStatus() StatusCode {
	e.indexMU.RLock()
	defer e.indexMU.RUnlock()
	sP := e.CurrentStatuses.sortByPriority()
	for _, v := range sP {
		if v.Status.Code != OK {
			return v.Status.Code
		}
	}
	return OK
}

func (e *EndpointStatus) String() string {
	return e.CurrentStatus().String()
}

// StringID returns the endpoint's ID in a string.
func (e *Endpoint) StringID() string {
	return strconv.Itoa(int(e.ID))
}

func (e *Endpoint) GetIdentity() identityPkg.NumericIdentity {
	if e.SecurityIdentity != nil {
		return e.SecurityIdentity.ID
	}

	return identityPkg.InvalidIdentity
}

func (e *Endpoint) directoryPath() string {
	return filepath.Join(".", fmt.Sprintf("%d", e.ID))
}

func (e *Endpoint) Allows(id identityPkg.NumericIdentity) bool {
	e.Mutex.RLock()
	defer e.Mutex.RUnlock()
	if e.Consumable != nil {
		return e.Consumable.AllowsIngress(id)
	}
	return false
}

// String returns endpoint on a JSON format.
func (e *Endpoint) String() string {
	e.Mutex.RLock()
	defer e.Mutex.RUnlock()
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// optionChanged is a callback used with pkg/option to apply the options to an
// endpoint.  Not used for anything at the moment.
func optionChanged(key string, value bool, data interface{}) {
}

// applyOptsLocked applies the given options to the endpoint's options and
// returns true if there were any options changed.
func (e *Endpoint) applyOptsLocked(opts map[string]string) bool {
	return e.Opts.Apply(opts, optionChanged, e) > 0
}

// ForcePolicyCompute marks the endpoint for forced bpf regeneration.
func (e *Endpoint) ForcePolicyCompute() {
	e.forcePolicyCompute = true
}

func (e *Endpoint) SetDefaultOpts(opts *option.BoolOptions) {
	if e.Opts == nil {
		e.Opts = option.NewBoolOptions(&EndpointOptionLibrary)
	}
	if e.Opts.Library == nil {
		e.Opts.Library = &EndpointOptionLibrary
	}

	if opts != nil {
		for k := range EndpointMutableOptionLibrary {
			e.Opts.Set(k, opts.IsEnabled(k))
		}
	}
}

// base64 returns the endpoint in a base64 format.
func (e *Endpoint) base64() (string, error) {
	var (
		jsonBytes []byte
		err       error
	)
	if e.Consumable != nil {
		e.Consumable.Mutex.RLock()
		jsonBytes, err = json.Marshal(e)
		e.Consumable.Mutex.RUnlock()
	} else {
		jsonBytes, err = json.Marshal(e)
	}
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(jsonBytes), nil
}

// parseBase64ToEndpoint parses the endpoint stored in the given base64 string.
func parseBase64ToEndpoint(str string, ep *Endpoint) error {
	jsonBytes, err := base64.StdEncoding.DecodeString(str)
	if err != nil {
		return err
	}
	return json.Unmarshal(jsonBytes, ep)
}

// FilterEPDir returns a list of directories' names that possible belong to an endpoint.
func FilterEPDir(dirFiles []os.FileInfo) []string {
	eptsID := []string{}
	for _, file := range dirFiles {
		if file.IsDir() {
			if _, err := strconv.ParseUint(file.Name(), 10, 16); err == nil {
				eptsID = append(eptsID, file.Name())
			}
		}
	}
	return eptsID
}

// ParseEndpoint parses the given strEp which is in the form of:
// common.CiliumCHeaderPrefix + common.Version + ":" + endpointBase64
func ParseEndpoint(strEp string) (*Endpoint, error) {
	// TODO: Provide a better mechanism to update from old version once we bump
	// TODO: cilium version.
	strEpSlice := strings.Split(strEp, ":")
	if len(strEpSlice) != 2 {
		return nil, fmt.Errorf("invalid format %q. Should contain a single ':'", strEp)
	}
	var ep Endpoint
	if err := parseBase64ToEndpoint(strEpSlice[1], &ep); err != nil {
		return nil, fmt.Errorf("failed to parse base64toendpoint: %s", err)
	}

	// We need to check for nil in Status, CurrentStatuses and Log, since in
	// some use cases, status will be not nil and Cilium will eventually
	// error/panic if CurrentStatus or Log are not initialized correctly.
	// Reference issue GH-2477
	if ep.Status == nil || ep.Status.CurrentStatuses == nil || ep.Status.Log == nil {
		ep.Status = NewEndpointStatus()
	}

	ep.state = StateRestoring

	return &ep, nil
}

func (e *Endpoint) LogStatus(typ StatusType, code StatusCode, msg string) {
	e.Mutex.Lock()
	defer e.Mutex.Unlock()
	// FIXME GH2323 instead of a mutex we could use a channel to send the status
	// log message to a single writer?
	e.Status.indexMU.Lock()
	defer e.Status.indexMU.Unlock()
	e.logStatusLocked(typ, code, msg)
}

func (e *Endpoint) LogStatusOK(typ StatusType, msg string) {
	e.LogStatus(typ, OK, msg)
}

// LogStatusOKLocked will log an OK message of the given status type with the
// given msg string.
// must be called with endpoint.Mutex held
func (e *Endpoint) LogStatusOKLocked(typ StatusType, msg string) {
	e.Status.indexMU.Lock()
	defer e.Status.indexMU.Unlock()
	e.logStatusLocked(typ, OK, msg)
}

func (e *Endpoint) logStatusLocked(typ StatusType, code StatusCode, msg string) {
	sts := &statusLogMsg{
		Status: Status{
			Code:  code,
			Msg:   msg,
			Type:  typ,
			State: e.state,
		},
		Timestamp: time.Now().UTC(),
	}
	e.Status.addStatusLog(sts)
	e.getLogger().WithFields(logrus.Fields{
		"code":                  sts.Status.Code,
		"type":                  sts.Status.Type,
		logfields.EndpointState: sts.Status.State,
	}).Debug(msg)
}

type UpdateValidationError struct {
	msg string
}

func (e UpdateValidationError) Error() string { return e.msg }

type UpdateCompilationError struct {
	msg string
}

func (e UpdateCompilationError) Error() string { return e.msg }

// UpdateStateChangeError is an error that indicates that updating the state
// of an endpoint was unsuccessful.
// Implements error interface.
type UpdateStateChangeError struct {
	msg string
}

func (e UpdateStateChangeError) Error() string { return e.msg }

// Update modifies the endpoint options and *always* tries to regenerate the
// endpoint's program. Returns an error if the provided options are not valid,
// if there was an issue triggering policy updates for the given endpoint,
// or if endpoint regeneration was unable to be triggered.
func (e *Endpoint) Update(owner Owner, cfg *models.EndpointConfigurationSpec) error {
	e.getLogger().WithField("configuration-options", cfg).Debug("updating endpoint configuration options")

	e.Mutex.Lock()
	if err := e.Opts.Validate(cfg.Options); err != nil {
		e.Mutex.Unlock()
		return UpdateValidationError{err.Error()}
	}

	// Option changes may be overridden by the policy configuration.
	// Currently we return all-OK even in that case.
	needToRegenerate, ctCleaned, err := e.TriggerPolicyUpdatesLocked(owner, cfg.Options)
	if err != nil {
		e.Mutex.Unlock()
		ctCleaned.Wait()
		return UpdateCompilationError{err.Error()}
	}

	reason := "endpoint was updated via API"

	// If configuration options are provided, we only regenerate if necessary.
	// Otherwise always regenerate.
	if cfg.Options == nil {
		needToRegenerate = true
		reason = "endpoint was manually regenerated via API"
	}

	if needToRegenerate {
		e.getLogger().Debug("need to regenerate endpoint; checking state before" +
			" attempting to regenerate")

		// TODO / FIXME: GH-3281: need ways to queue up regenerations per-endpoint.

		// Default timeout for PATCH /endpoint/{id}/config is 30 seconds, so put
		// timeout in this function a bit below that timeout. If the timeout
		// for clients in API is below this value, they will get a message containing
		// "context deadline exceeded".
		stateChangeTimeout := time.Duration(25 * time.Second)

		// Check up until stateChangeTimeout seconds for endpoint state before
		// trying to update configuration.
		timeout := time.After(stateChangeTimeout)

		// Check for endpoint state every second.
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		e.Mutex.Unlock()
		for {
			select {
			case <-ticker.C:
				e.Mutex.Lock()
				// Check endpoint state before attempting configuration update because
				// configuration updates can only be applied when the endpoint is in
				// specific states. See GH-3058.
				stateTransitionSucceeded := e.SetStateLocked(StateWaitingToRegenerate, reason)
				if stateTransitionSucceeded {
					e.Mutex.Unlock()
					ctCleaned.Wait()
					e.Regenerate(owner, reason)
					return nil
				}
				e.Mutex.Unlock()
			case <-timeout:
				e.Mutex.Lock()
				e.getLogger().Warningf("timed out waiting for endpoint state to change")
				e.Mutex.Unlock()
				ctCleaned.Wait()
				return UpdateStateChangeError{fmt.Sprintf("unable to regenerate endpoint program because state transition to %s was unsuccessful; check `cilium endpoint log %d` for more information", StateWaitingToRegenerate, e.ID)}
			}
		}

	}

	e.Mutex.Unlock()
	ctCleaned.Wait()

	return nil
}

// HasLabels returns whether endpoint e contains all labels l. Will return 'false'
// if any label in l is not in the endpoint's labels.
func (e *Endpoint) HasLabels(l pkgLabels.Labels) bool {
	e.Mutex.RLock()
	defer e.Mutex.RUnlock()
	allEpLabels := e.OpLabels.AllLabels()

	for _, v := range l {
		found := false
		for _, j := range allEpLabels {
			if j.Equals(v) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	return true
}

func (e *Endpoint) replaceInformationLabels(l pkgLabels.Labels) {
	e.Mutex.Lock()
	for k, v := range l {
		tmp := v.DeepCopy()
		e.getLogger().WithField(logfields.Labels, logfields.Repr(tmp)).Debug("Assigning information label")
		e.OpLabels.OrchestrationInfo[k] = tmp
	}
	e.Mutex.Unlock()
}

// replaceIdentityLabels replaces the identity labels of an endpoint. If a net
// changed occurred, the identityRevision is bumped and return, otherwise 0 is
// returned.
func (e *Endpoint) replaceIdentityLabels(l pkgLabels.Labels) int {
	e.Mutex.Lock()
	changed := false

	e.OpLabels.OrchestrationIdentity.MarkAllForDeletion()
	e.OpLabels.Disabled.MarkAllForDeletion()

	for k, v := range l {
		switch {
		case e.OpLabels.Disabled[k] != nil:
			e.OpLabels.Disabled[k].DeletionMark = false

		case e.OpLabels.OrchestrationIdentity[k] != nil:
			e.OpLabels.OrchestrationIdentity[k].DeletionMark = false

		default:
			tmp := v.DeepCopy()
			e.getLogger().WithField(logfields.Labels, logfields.Repr(tmp)).Debug("Assigning security relevant label")
			e.OpLabels.OrchestrationIdentity[k] = tmp
			changed = true
		}
	}

	if e.OpLabels.OrchestrationIdentity.DeleteMarked() || e.OpLabels.Disabled.DeleteMarked() {
		changed = true
	}

	rev := 0
	if changed {
		e.identityRevision++
		rev = e.identityRevision
	}

	e.Mutex.Unlock()

	return rev
}

// LeaveLocked removes the endpoint's directory from the system. Must be called
// with Endpoint's mutex AND BuildMutex locked.
func (e *Endpoint) LeaveLocked(owner Owner) []error {
	errors := []error{}

	owner.RemoveFromEndpointQueue(uint64(e.ID))
	if c := e.Consumable; c != nil {
		c.Mutex.Lock()
		if e.L4Policy != nil {
			// Passing a new map of nil will purge all redirects
			e.removeOldRedirects(owner, nil)
		}
		c.Mutex.Unlock()
	}

	if e.PolicyMap != nil {
		if err := e.PolicyMap.Close(); err != nil {
			errors = append(errors, fmt.Errorf("unable to close policymap %s: %s", e.PolicyGlobalMapPathLocked(), err))
		}
	}

	if e.SecurityIdentity != nil {
		err := e.SecurityIdentity.Release()
		if err != nil {
			errors = append(errors, fmt.Errorf("unable to release identity: %s", err))
		}
		// TODO: Check if network policy was created even without SecurityIdentity
		owner.RemoveNetworkPolicy(e)
		e.SecurityIdentity = nil
	}

	e.L3Maps.Close()
	e.removeDirectory()
	e.controllers.RemoveAll()
	e.cleanPolicySignals()

	e.SetStateLocked(StateDisconnected, "Endpoint removed")

	return errors
}

func (e *Endpoint) removeDirectory() {
	os.RemoveAll(e.directoryPath())
}

func (e *Endpoint) RemoveDirectory() {
	e.Mutex.Lock()
	defer e.Mutex.Unlock()
	e.removeDirectory()
}

func (e *Endpoint) CreateDirectory() error {
	e.Mutex.Lock()
	defer e.Mutex.Unlock()
	lxcDir := e.directoryPath()
	if err := os.MkdirAll(lxcDir, 0777); err != nil {
		return fmt.Errorf("unable to create endpoint directory: %s", err)
	}

	return nil
}

// RegenerateWait should only be called when endpoint's state has successfully
// been changed to "waiting-to-regenerate"
func (e *Endpoint) RegenerateWait(owner Owner, reason string) error {
	if !<-e.Regenerate(owner, reason) {
		return fmt.Errorf("error while regenerating endpoint."+
			" For more info run: 'cilium endpoint get %d'", e.ID)
	}
	return nil
}

// SetContainerName modifies the endpoint's container name
func (e *Endpoint) SetContainerName(name string) {
	e.Mutex.Lock()
	e.ContainerName = name
	e.Mutex.Unlock()
}

// GetK8sNamespace returns the name of the pod if the endpoint represents a
// Kubernetes pod
func (e *Endpoint) GetK8sNamespace() string {
	e.Mutex.RLock()
	defer e.Mutex.RUnlock()

	return e.k8sNamespace
}

// SetK8sNamespace modifies the endpoint's pod name
func (e *Endpoint) SetK8sNamespace(name string) {
	e.Mutex.Lock()
	e.k8sNamespace = name
	e.Mutex.Unlock()
}

// GetK8sPodName returns the name of the pod if the endpoint represents a
// Kubernetes pod
func (e *Endpoint) GetK8sPodName() string {
	e.Mutex.RLock()
	defer e.Mutex.RUnlock()

	return e.k8sPodName
}

// GetK8sNamespaceAndPodNameLocked returns the namespace and pod name.  This
// function requires e.Mutex to be held.
func (e *Endpoint) GetK8sNamespaceAndPodNameLocked() string {
	return e.k8sNamespace + ":" + e.k8sPodName
}

// SetK8sPodName modifies the endpoint's pod name
func (e *Endpoint) SetK8sPodName(name string) {
	e.Mutex.Lock()
	e.k8sPodName = name
	e.Mutex.Unlock()
}

// SetContainerID modifies the endpoint's container ID
func (e *Endpoint) SetContainerID(id string) {
	e.Mutex.Lock()
	e.DockerID = id
	e.Mutex.Unlock()
}

// GetContainerID returns the endpoint's container ID
func (e *Endpoint) GetContainerID() string {
	e.Mutex.RLock()
	id := e.DockerID
	e.Mutex.RUnlock()
	return id
}

// GetShortContainerID returns the endpoint's shortened container ID
func (e *Endpoint) GetShortContainerID() string {
	e.Mutex.RLock()
	id := e.getShortContainerID()
	e.Mutex.RUnlock()
	return id
}

func (e *Endpoint) getShortContainerID() string {
	if e == nil {
		return ""
	}

	caplen := 10
	if len(e.DockerID) <= caplen {
		return e.DockerID
	}

	return e.DockerID[:caplen]

}

// SetDockerEndpointID modifies the endpoint's Docker Endpoint ID
func (e *Endpoint) SetDockerEndpointID(id string) {
	e.Mutex.Lock()
	e.DockerEndpointID = id
	e.Mutex.Unlock()
}

// SetDockerNetworkID modifies the endpoint's Docker Endpoint ID
func (e *Endpoint) SetDockerNetworkID(id string) {
	e.Mutex.Lock()
	e.DockerNetworkID = id
	e.Mutex.Unlock()
}

// GetDockerNetworkID returns the endpoint's Docker Endpoint ID
func (e *Endpoint) GetDockerNetworkID() string {
	e.Mutex.RLock()
	id := e.DockerNetworkID
	e.Mutex.RUnlock()

	return id
}

// bumpPolicyRevision marks the endpoint to be running the next scheduled
// policy revision as setup by e.regenerate(). endpoint.Mutex should not be held.
func (e *Endpoint) bumpPolicyRevision(revision uint64) {
	e.Mutex.Lock()
	if revision > e.policyRevision {
		e.setPolicyRevision(revision)
	}
	e.Mutex.Unlock()
}

// OnProxyPolicyUpdate is a callback used to update the Endpoint's
// proxyPolicyRevision when the specified revision has been applied in the
// proxy.
func (e *Endpoint) OnProxyPolicyUpdate(revision uint64) {
	e.Mutex.Lock()
	if revision > e.proxyPolicyRevision {
		e.proxyPolicyRevision = revision
	}
	e.Mutex.Unlock()
}

// getProxyStatisticsLocked gets the ProxyStatistics for the flows with the
// given characteristics, or adds a new one and returns it.
// Must be called with e.proxyStatisticsMutex held.
func (e *Endpoint) getProxyStatisticsLocked(l7Protocol string, port uint16, ingress bool) *models.ProxyStatistics {
	var location string
	if ingress {
		location = models.ProxyStatisticsLocationIngress
	} else {
		location = models.ProxyStatisticsLocationEgress
	}
	key := models.ProxyStatistics{
		Location: location,
		Port:     int64(port),
		Protocol: l7Protocol,
	}

	if e.proxyStatistics == nil {
		e.proxyStatistics = make(map[models.ProxyStatistics]*models.ProxyStatistics)
	}

	proxyStats, ok := e.proxyStatistics[key]
	if !ok {
		keyCopy := key
		proxyStats = &keyCopy
		proxyStats.Statistics = &models.RequestResponseStatistics{
			Requests:  &models.MessageForwardingStatistics{},
			Responses: &models.MessageForwardingStatistics{},
		}
		e.proxyStatistics[key] = proxyStats
	}

	return proxyStats
}

// UpdateProxyStatistics updates the Endpoint's proxy  statistics to account
// for a new observed flow with the given characteristics.
func (e *Endpoint) UpdateProxyStatistics(l7Protocol string, port uint16, ingress, request bool, verdict accesslog.FlowVerdict) {
	e.proxyStatisticsMutex.Lock()
	defer e.proxyStatisticsMutex.Unlock()

	proxyStats := e.getProxyStatisticsLocked(l7Protocol, port, ingress)

	var stats *models.MessageForwardingStatistics
	if request {
		stats = proxyStats.Statistics.Requests
	} else {
		stats = proxyStats.Statistics.Responses
	}

	stats.Received++

	switch verdict {
	case accesslog.VerdictForwarded:
		stats.Forwarded++
	case accesslog.VerdictDenied:
		stats.Denied++
	case accesslog.VerdictError:
		stats.Error++
	}
}

// APICanModify determines whether API requests from a user are allowed to
// modify this endpoint.
func APICanModify(e *Endpoint) error {
	if lbls := e.OpLabels.OrchestrationIdentity.FindReserved(); lbls != nil {
		return fmt.Errorf("Endpoint cannot be modified by API call")
	}
	return nil
}

func (e *Endpoint) getIDandLabels() string {
	e.Mutex.RLock()
	defer e.Mutex.RUnlock()

	labels := ""
	if e.SecurityIdentity != nil {
		labels = e.SecurityIdentity.Labels.String()
	}

	return fmt.Sprintf("%d (%s)", e.ID, labels)
}

// ModifyIdentityLabels changes the identity relevant labels of an endpoint.
// labels can be added or deleted. If a net label changed is performed, the
// endpoint will receive a new identity and will be regenerated. Both of these
// operations will happen in the background.
func (e *Endpoint) ModifyIdentityLabels(owner Owner, addLabels, delLabels labels.Labels) error {
	e.Mutex.Lock()
	defer e.Mutex.Unlock()

	newLabels := e.OpLabels.DeepCopy()

	if len(delLabels) > 0 {
		for k := range delLabels {
			// The change request is accepted if the label is on
			// any of the lists. If the label is already disabled,
			// we will simply ignore that change.
			if newLabels.OrchestrationIdentity[k] != nil ||
				newLabels.Custom[k] != nil ||
				newLabels.Disabled[k] != nil {
				break
			}

			return fmt.Errorf("label %s not found", k)
		}
	}

	if len(delLabels) > 0 {
		for k, v := range delLabels {
			if newLabels.OrchestrationIdentity[k] != nil {
				delete(newLabels.OrchestrationIdentity, k)
				newLabels.Disabled[k] = v
			}

			if newLabels.Custom[k] != nil {
				delete(newLabels.Custom, k)
			}
		}
	}

	if len(addLabels) > 0 {
		for k, v := range addLabels {
			if newLabels.Disabled[k] != nil {
				delete(newLabels.Disabled, k)
				newLabels.OrchestrationIdentity[k] = v
			} else if newLabels.OrchestrationIdentity[k] == nil {
				newLabels.Custom[k] = v
			}
		}
	}

	e.OpLabels = *newLabels

	// Mark with StateWaitingForIdentity, it will be set to
	// StateWaitingToRegenerate after the identity resolution has been
	// completed
	e.SetStateLocked(StateWaitingForIdentity, "Triggering identity resolution due to updated security labels")

	e.identityRevision++
	rev := e.identityRevision

	e.runLabelsResolver(owner, rev)

	return nil
}

// UpdateLabels is called to update the labels of an endpoint. Calls to this
// function do not necessarily mean that the labels actually changed. The
// container runtime layer will periodically synchronize labels.
//
// If a net label changed was performed, the endpoint will receive a new
// identity and will be regenerated. Both of these operations will happen in
// the background.
func (e *Endpoint) UpdateLabels(owner Owner, identityLabels, infoLabels labels.Labels) {
	log.WithFields(logrus.Fields{
		logfields.ContainerID:    e.GetShortContainerID(),
		logfields.EndpointID:     e.StringID(),
		logfields.IdentityLabels: identityLabels.String(),
		logfields.InfoLabels:     infoLabels.String(),
	}).Debug("Refreshing labels of endpoint")

	e.replaceInformationLabels(infoLabels)

	// replace identity labels and update the identity if labels have changed
	if rev := e.replaceIdentityLabels(identityLabels); rev != 0 {
		e.runLabelsResolver(owner, rev)
	}
}

// setPolicyRevision sets the policy wantedRev with the given revision.
func (e *Endpoint) setPolicyRevision(rev uint64) {
	e.policyRevision = rev
	for ps := range e.policyRevisionSignals {
		select {
		case <-ps.ctx.Done():
			close(ps.ch)
			delete(e.policyRevisionSignals, ps)
		default:
			if rev >= ps.wantedRev {
				close(ps.ch)
				delete(e.policyRevisionSignals, ps)
			}
		}
	}
}

// cleanPolicySignals closes and removes all policy revision signals.
func (e *Endpoint) cleanPolicySignals() {
	for w := range e.policyRevisionSignals {
		close(w.ch)
	}
	e.policyRevisionSignals = map[policySignal]bool{}
}

// policySignal is used to mark when a wanted policy wantedRev is reached
type policySignal struct {
	// wantedRev specifies which policy revision the signal wants.
	wantedRev uint64
	// ch is the channel that signalizes once the policy revision wanted is reached.
	ch chan struct{}
	// ctx is the context for the policy signal request.
	ctx context.Context
}

// WaitForPolicyRevision returns a channel that is closed when one or more of
// the following conditions have met:
//  - the endpoint is disconnected state
//  - the endpoint's policy revision reaches the wanted revision
func (e *Endpoint) WaitForPolicyRevision(ctx context.Context, rev uint64) <-chan struct{} {
	e.Mutex.Lock()
	defer e.Mutex.Unlock()
	ch := make(chan struct{})
	if e.policyRevision >= rev || e.state == StateDisconnected {
		close(ch)
		return ch
	}
	ps := policySignal{
		wantedRev: rev,
		ctx:       ctx,
		ch:        ch,
	}
	if e.policyRevisionSignals == nil {
		e.policyRevisionSignals = map[policySignal]bool{}
	}
	e.policyRevisionSignals[ps] = true
	return ch
}

// IPs returns the slice of valid IPs for this endpoint.
func (e *Endpoint) IPs() []net.IP {
	ips := []net.IP{}
	if e.IPv4 != nil {
		ips = append(ips, e.IPv4.IP())
	}
	if e.IPv6 != nil {
		ips = append(ips, e.IPv6.IP())
	}
	return ips
}
