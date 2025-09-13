package websocket

import (
	"encoding/json"
	"time"

	v1 "k8s.io/api/core/v1"
)

// MessageType represents the type of WebSocket message
type MessageType string

const (
	// Commands from backend to kubelet
	CommandDeploy    MessageType = "deploy"
	CommandTerminate MessageType = "terminate"
	CommandStatus    MessageType = "status"
	CommandPing      MessageType = "ping"

	// Events from kubelet to backend
	EventPodCreated      MessageType = "pod_created"
	EventPodStatusChange MessageType = "pod_status_change"
	EventPodDeleted      MessageType = "pod_deleted"
	EventPong            MessageType = "pong"

	// Responses from kubelet to backend
	ResponseResult MessageType = "result"
	ResponseError  MessageType = "error"
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

// Message is the base message structure for WebSocket communication
type Message struct {
	ID        string      `json:"id"`
	Type      MessageType `json:"type"`
	Timestamp time.Time   `json:"timestamp"`
	Data      interface{} `json:"data"`
}

// Command represents a command sent from backend to kubelet
type Command struct {
	ID       string       `json:"id"`
	Type     MessageType  `json:"type"`
	Provider ProviderType `json:"provider"`
	PodID    string       `json:"pod_id"`
	Params   interface{}  `json:"params"`
}

// Response represents a response sent from kubelet to backend
type Response struct {
	CommandID string      `json:"command_id"`
	Success   bool        `json:"success"`
	Result    interface{} `json:"result,omitempty"`
	Error     string      `json:"error,omitempty"`
}

// Event represents an unsolicited event sent from kubelet to backend
type Event struct {
	Type      MessageType `json:"type"`
	PodID     string      `json:"pod_id"`
	Namespace string      `json:"namespace,omitempty"`
	Data      interface{} `json:"data"`
}

// DeployParams represents parameters for pod deployment
type DeployParams struct {
	Image              string            `json:"image"`
	GPUTypeIDs         []string          `json:"gpu_type_ids,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	Ports              []string          `json:"ports,omitempty"`
	ContainerDiskInGb  int               `json:"container_disk_in_gb,omitempty"`
	VolumeInGb         int               `json:"volume_in_gb,omitempty"`
	MinRAMPerGPU       int               `json:"min_ram_per_gpu,omitempty"`
	CloudType          string            `json:"cloud_type,omitempty"`
	DatacenterIDs      []string          `json:"datacenter_ids,omitempty"`
	TemplateID         string            `json:"template_id,omitempty"`
	ContainerAuthID    string            `json:"container_auth_id,omitempty"`
	MaxPrice           float64           `json:"max_price,omitempty"`
	Name               string            `json:"name"`
}

// DeployResult represents the result of a pod deployment
type DeployResult struct {
	ProviderPodID string  `json:"provider_pod_id"`
	CostPerHour   float64 `json:"cost_per_hour"`
	MachineID     string  `json:"machine_id,omitempty"`
	Status        string  `json:"status"`
	Location      string  `json:"location,omitempty"`
	DatacenterID  string  `json:"datacenter_id,omitempty"`
}

// TerminateParams represents parameters for pod termination
type TerminateParams struct {
	ProviderPodID string `json:"provider_pod_id"`
}

// StatusParams represents parameters for status check
type StatusParams struct {
	ProviderPodID string `json:"provider_pod_id"`
}

// StatusResult represents the result of a status check
type StatusResult struct {
	Status        string    `json:"status"`
	ExitCode      int       `json:"exit_code,omitempty"`
	Message       string    `json:"message,omitempty"`
	LastUpdated   time.Time `json:"last_updated"`
	IsRunning     bool      `json:"is_running"`
	IsTerminated  bool      `json:"is_terminated"`
	IsSuccessful  bool      `json:"is_successful"`
}

// PodCreatedEvent represents a pod creation event
type PodCreatedEvent struct {
	PodSpec     *v1.Pod           `json:"pod_spec"`
	Annotations map[string]string `json:"annotations"`
}

// PodStatusChangeEvent represents a pod status change event
type PodStatusChangeEvent struct {
	ProviderPodID string `json:"provider_pod_id,omitempty"`
	OldStatus     string `json:"old_status"`
	NewStatus     string `json:"new_status"`
	Message       string `json:"message,omitempty"`
	ExitCode      int    `json:"exit_code,omitempty"`
}

// PodDeletedEvent represents a pod deletion event
type PodDeletedEvent struct {
	ProviderPodID string `json:"provider_pod_id,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// NewMessage creates a new message with timestamp
func NewMessage(id string, msgType MessageType, data interface{}) *Message {
	return &Message{
		ID:        id,
		Type:      msgType,
		Timestamp: time.Now(),
		Data:      data,
	}
}

// ToJSON serializes a message to JSON
func (m *Message) ToJSON() ([]byte, error) {
	return json.Marshal(m)
}

// FromJSON deserializes a message from JSON
func FromJSON(data []byte) (*Message, error) {
	var msg Message
	err := json.Unmarshal(data, &msg)
	return &msg, err
}

// ParseCommand extracts command data from a message
func (m *Message) ParseCommand() (*Command, error) {
	data, err := json.Marshal(m.Data)
	if err != nil {
		return nil, err
	}

	var cmd Command
	err = json.Unmarshal(data, &cmd)
	return &cmd, err
}

// ParseResponse extracts response data from a message
func (m *Message) ParseResponse() (*Response, error) {
	data, err := json.Marshal(m.Data)
	if err != nil {
		return nil, err
	}

	var resp Response
	err = json.Unmarshal(data, &resp)
	return &resp, err
}

// ParseEvent extracts event data from a message
func (m *Message) ParseEvent() (*Event, error) {
	data, err := json.Marshal(m.Data)
	if err != nil {
		return nil, err
	}

	var event Event
	err = json.Unmarshal(data, &event)
	return &event, err
}