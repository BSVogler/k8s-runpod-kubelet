// Package websocket implements the Conduit kubelet WebSocket protocol v2.
//
// Every frame in both directions is an Envelope. The envelope type decides how
// the payload is decoded:
//
//	command              service -> kubelet   payload: Command   (envelope.id is the command id)
//	response             kubelet -> service   payload: Response
//	event                kubelet -> service   payload: Event
//	heartbeat            kubelet -> service   payload: Heartbeat (every 30s)
//	kubelet_registration kubelet -> service   payload: Registration (once after connect)
//
// Field names are snake_case JSON. Unknown fields are ignored. Optional fields
// are omitted rather than sent as null.
package websocket

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
)

// ProtocolVersion is reported in the registration metadata.
const ProtocolVersion = "2.0.0"

// KubeletType is the registration "type" field.
const KubeletType = "conduit-kubelet"

// EnvelopeType is the top-level message type.
type EnvelopeType string

const (
	EnvelopeCommand      EnvelopeType = "command"
	EnvelopeResponse     EnvelopeType = "response"
	EnvelopeEvent        EnvelopeType = "event"
	EnvelopeHeartbeat    EnvelopeType = "heartbeat"
	EnvelopeRegistration EnvelopeType = "kubelet_registration"
)

// CommandType identifies a command sent from the service to the kubelet.
type CommandType string

const (
	CommandDeploy    CommandType = "deploy"
	CommandTerminate CommandType = "terminate"
	CommandStatus    CommandType = "status"
	CommandPing      CommandType = "ping"
	CommandReject    CommandType = "reject"
)

// EventType identifies an event sent from the kubelet to the service.
type EventType string

const (
	EventPodCreated      EventType = "pod_created"
	EventPodDeleted      EventType = "pod_deleted"
	EventPodStatusChange EventType = "pod_status_change"
	EventKubeletReady    EventType = "kubelet_ready"
	EventKubeletError    EventType = "kubelet_error"
)

// ResponseStatus is the outcome of a command.
type ResponseStatus string

const (
	StatusSuccess ResponseStatus = "success"
	StatusError   ResponseStatus = "error"
)

// ProviderType represents the cloud provider
type ProviderType string

const (
	ProviderRunPod ProviderType = "runpod"
	ProviderVastAI ProviderType = "vastai"
	ProviderSalad  ProviderType = "salad"
	ProviderAWS    ProviderType = "aws"
	ProviderGCP    ProviderType = "gcp"
)

// Reject codes sent by the platform.
const (
	RejectQuotaExceeded       = "quota_exceeded"
	RejectNoAPIKey            = "no_api_key"
	RejectUnsupportedProvider = "unsupported_provider"
	RejectProviderError       = "provider_error"
	RejectConversionError     = "conversion_error"
)

// Timestamp is a time.Time that serializes as RFC3339 (UTC) and tolerates
// timezone-less ISO-8601 input (Python's datetime.utcnow().isoformat()),
// which is treated as UTC.
type Timestamp struct {
	time.Time
}

// Now returns the current time as a Timestamp.
func Now() Timestamp {
	return Timestamp{time.Now().UTC()}
}

// MarshalJSON implements json.Marshaler.
func (t Timestamp) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return json.Marshal(time.Now().UTC().Format(time.RFC3339Nano))
	}
	return json.Marshal(t.UTC().Format(time.RFC3339Nano))
}

// UnmarshalJSON implements json.Unmarshaler.
func (t *Timestamp) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("timestamp must be a string: %w", err)
	}
	if s == "" {
		t.Time = time.Time{}
		return nil
	}

	if parsed, err := time.Parse(time.RFC3339Nano, s); err == nil {
		t.Time = parsed
		return nil
	}

	// Lenient fallback: naive ISO-8601 without offset, assumed UTC.
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		if parsed, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			t.Time = parsed
			return nil
		}
	}

	return fmt.Errorf("unrecognized timestamp format: %q", s)
}

// Envelope is the top-level frame for every WebSocket message.
type Envelope struct {
	ID        string          `json:"id"`
	Type      EnvelopeType    `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	Timestamp Timestamp       `json:"timestamp"`
	KubeletID string          `json:"kubelet_id,omitempty"`
}

// NewEnvelope wraps payload in an envelope with a fresh id and timestamp.
func NewEnvelope(envType EnvelopeType, payload interface{}, kubeletID string) (*Envelope, error) {
	return NewEnvelopeWithID(uuid.NewString(), envType, payload, kubeletID)
}

// NewEnvelopeWithID wraps payload in an envelope with the given id.
func NewEnvelopeWithID(id string, envType EnvelopeType, payload interface{}, kubeletID string) (*Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal %s payload: %w", envType, err)
	}
	return &Envelope{
		ID:        id,
		Type:      envType,
		Payload:   raw,
		Timestamp: Now(),
		KubeletID: kubeletID,
	}, nil
}

// ToJSON serializes the envelope.
func (e *Envelope) ToJSON() ([]byte, error) {
	return json.Marshal(e)
}

// ParseEnvelope deserializes an envelope from JSON.
func ParseEnvelope(data []byte) (*Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("failed to parse envelope: %w", err)
	}
	if env.Type == "" {
		return nil, fmt.Errorf("envelope has no type")
	}
	return &env, nil
}

// DecodePayload unmarshals the envelope payload into v.
func (e *Envelope) DecodePayload(v interface{}) error {
	if len(e.Payload) == 0 || string(e.Payload) == "null" {
		return fmt.Errorf("envelope %s has empty payload", e.Type)
	}
	if err := json.Unmarshal(e.Payload, v); err != nil {
		return fmt.Errorf("failed to decode %s payload: %w", e.Type, err)
	}
	return nil
}

// Command is the payload of a "command" envelope (service -> kubelet).
type Command struct {
	Type CommandType     `json:"type"`
	Data json.RawMessage `json:"data"`
}

// NewCommand builds a command with data marshalled into place.
func NewCommand(cmdType CommandType, data interface{}) (*Command, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal %s command data: %w", cmdType, err)
	}
	return &Command{Type: cmdType, Data: raw}, nil
}

// DecodeData unmarshals the command data into v.
func (c *Command) DecodeData(v interface{}) error {
	if len(c.Data) == 0 || string(c.Data) == "null" {
		return fmt.Errorf("%s command has no data", c.Type)
	}
	if err := json.Unmarshal(c.Data, v); err != nil {
		return fmt.Errorf("failed to decode %s command data: %w", c.Type, err)
	}
	return nil
}

// ErrorDetails is the structured error attached to a failed response.
type ErrorDetails struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	ProviderError string `json:"provider_error,omitempty"`
}

// Response is the payload of a "response" envelope (kubelet -> service).
// Data and Error are sent as null when unset, as the spec requires.
type Response struct {
	CommandID string         `json:"command_id"`
	Status    ResponseStatus `json:"status"`
	Data      interface{}    `json:"data"`
	Error     *ErrorDetails  `json:"error"`
}

// NewSuccessResponse builds a success response.
func NewSuccessResponse(commandID string, data interface{}) *Response {
	if data == nil {
		data = map[string]interface{}{}
	}
	return &Response{CommandID: commandID, Status: StatusSuccess, Data: data}
}

// NewErrorResponse builds an error response.
func NewErrorResponse(commandID, code, message, providerError string) *Response {
	return &Response{
		CommandID: commandID,
		Status:    StatusError,
		Error:     &ErrorDetails{Code: code, Message: message, ProviderError: providerError},
	}
}

// Success reports whether the response carries a success status.
func (r *Response) Success() bool {
	return r != nil && r.Status == StatusSuccess
}

// Event is the payload of an "event" envelope (kubelet -> service).
type Event struct {
	Type EventType   `json:"type"`
	Data interface{} `json:"data"`
}

// Heartbeat is the payload of a "heartbeat" envelope.
type Heartbeat struct {
	Timestamp Timestamp `json:"timestamp"`
	KubeletID string    `json:"kubelet_id"`
	Status    string    `json:"status"`
}

// NewHeartbeat builds an "alive" heartbeat.
func NewHeartbeat(kubeletID string) *Heartbeat {
	return &Heartbeat{Timestamp: Now(), KubeletID: kubeletID, Status: "alive"}
}

// RegistrationMetadata is the metadata block of the registration payload.
type RegistrationMetadata struct {
	Version    string `json:"version"`
	InternalIP string `json:"internal_ip"`
	KeyMode    string `json:"key_mode"` // "local" | "platform"
}

// Registration is the payload of a "kubelet_registration" envelope.
type Registration struct {
	ID           string               `json:"id"`
	Type         string               `json:"type"`
	ClusterName  string               `json:"cluster_name"`
	NodeName     string               `json:"node_name"`
	Namespace    string               `json:"namespace"`
	Capabilities []string             `json:"capabilities"`
	Metadata     RegistrationMetadata `json:"metadata"`
}

// DeployParams is the data of a "deploy" command. The platform has already
// converted the pod spec into flat, provider-specific parameters.
type DeployParams struct {
	PodName   string `json:"pod_name"`
	Namespace string `json:"namespace"`
	Provider  string `json:"provider"`

	// Platform-managed API key (SaaS mode). Absent => use the local provider key.
	APIKey string `json:"api_key,omitempty"`

	Image             string            `json:"image"`
	Name              string            `json:"name"`
	Env               map[string]string `json:"env,omitempty"`
	Ports             []string          `json:"ports,omitempty"`
	ContainerDiskInGb int               `json:"container_disk_in_gb,omitempty"`
	VolumeInGb        int               `json:"volume_in_gb,omitempty"`
	GPUTypeIDs        []string          `json:"gpu_type_ids,omitempty"`
	GPUCount          int               `json:"gpu_count,omitempty"`
	MinRAMPerGPU      int               `json:"min_ram_per_gpu,omitempty"`
	CloudType         string            `json:"cloud_type,omitempty"`
	DatacenterIDs     []string          `json:"datacenter_ids,omitempty"`
	TemplateID        string            `json:"template_id,omitempty"`
	ContainerAuthID   string            `json:"container_auth_id,omitempty"`
	MaxPrice          float64           `json:"max_price,omitempty"`
	Command           []string          `json:"command,omitempty"`
	Args              []string          `json:"args,omitempty"`
}

// DeployResult is the response data of a successful "deploy" command.
type DeployResult struct {
	ProviderPodID string            `json:"provider_pod_id"`
	Provider      string            `json:"provider"`
	Status        string            `json:"status"`
	CostPerHour   float64           `json:"cost_per_hour"`
	GPUType       string            `json:"gpu_type,omitempty"`
	MachineID     string            `json:"machine_id,omitempty"`
	DatacenterID  string            `json:"datacenter_id,omitempty"`
	Ports         map[string]string `json:"ports,omitempty"` // port -> external URL
}

// TerminateParams is the data of a "terminate" command.
type TerminateParams struct {
	PodName       string `json:"pod_name"`
	Namespace     string `json:"namespace"`
	Provider      string `json:"provider"`
	ProviderPodID string `json:"provider_pod_id"`
	APIKey        string `json:"api_key,omitempty"`
}

// TerminateResult is the response data of a successful "terminate" command.
type TerminateResult struct {
	Terminated bool `json:"terminated"`
}

// StatusParams is the data of a "status" command.
type StatusParams struct {
	PodName       string `json:"pod_name"`
	Namespace     string `json:"namespace"`
	Provider      string `json:"provider"`
	ProviderPodID string `json:"provider_pod_id"`
	APIKey        string `json:"api_key,omitempty"`
}

// StatusResult is the response data of a successful "status" command.
type StatusResult struct {
	PodName       string `json:"pod_name"`
	ProviderPodID string `json:"provider_pod_id"`
	Provider      string `json:"provider"`
	Status        string `json:"status"` // provider status, standardized (RUNNING, EXITED, ...)
	Phase         string `json:"phase"`  // Kubernetes pod phase (Pending, Running, Succeeded, Failed)
	IsRunning     bool   `json:"is_running"`
	IsTerminated  bool   `json:"is_terminated"`
	ExitCode      int    `json:"exit_code"`
	Message       string `json:"message,omitempty"`
}

// RejectParams is the data of a "reject" command: the platform refuses to run
// the pod and the kubelet marks it Failed.
type RejectParams struct {
	PodName    string `json:"pod_name"`
	Namespace  string `json:"namespace"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	UpgradeURL string `json:"upgrade_url,omitempty"`
}

// PodCreatedEvent is the data of a "pod_created" event.
type PodCreatedEvent struct {
	PodName     string            `json:"pod_name"`
	Namespace   string            `json:"namespace"`
	UID         string            `json:"uid"`
	PodSpec     *v1.Pod           `json:"pod_spec"`
	Annotations map[string]string `json:"annotations"`
}

// PodDeletedEvent is the data of a "pod_deleted" event.
type PodDeletedEvent struct {
	PodName       string `json:"pod_name"`
	Namespace     string `json:"namespace"`
	UID           string `json:"uid"`
	ProviderPodID string `json:"provider_pod_id,omitempty"`
	Reason        string `json:"reason"`
}

// PodStatusChangeEvent is the data of a "pod_status_change" event.
type PodStatusChangeEvent struct {
	PodName       string `json:"pod_name"`
	Namespace     string `json:"namespace"`
	ProviderPodID string `json:"provider_pod_id,omitempty"`
	OldStatus     string `json:"old_status"`
	NewStatus     string `json:"new_status"`
	Message       string `json:"message,omitempty"`
	ExitCode      *int   `json:"exit_code,omitempty"`
}

// KubeletReadyEvent is the data of a "kubelet_ready" event.
type KubeletReadyEvent struct {
	NodeName string `json:"node_name"`
}

// KubeletErrorEvent is the data of a "kubelet_error" event.
type KubeletErrorEvent struct {
	Message string `json:"message"`
}

// PodKey returns the "namespace/name" key used to track pods.
func PodKey(namespace, name string) string {
	return strings.TrimSpace(namespace) + "/" + strings.TrimSpace(name)
}
