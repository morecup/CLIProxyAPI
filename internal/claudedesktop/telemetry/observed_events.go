package telemetry

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	maxObservedEventFieldsBytes     = 128 << 10
	maxObservedCrashAttachmentBytes = 3 << 20
)

var ErrInvalidObservation = errors.New("invalid Claude Desktop telemetry observation")

func invalidObserved(format string, values ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidObservation, fmt.Sprintf(format, values...))
}

type ObservedRendererEvent struct {
	Kind            string
	Route           string
	SessionID       string
	PromptID        string
	ClientRequestID string
	Properties      map[string]any
}

type ObservedMainProcessEvent struct {
	Kind            string
	SessionID       string
	ClientRequestID string
	Metadata        map[string]any
}

type ObservedSDKEvent struct {
	Kind            string
	SessionID       string
	Model           string
	PromptID        string
	ClientRequestID string
	SkillName       string
	Metadata        map[string]any
}

type ObservedPerformanceEvent struct {
	Kind            string
	SessionID       string
	ClientRequestID string
	Data            map[string]any
}

type ObservedCrashAttachment struct {
	Filename string
	Data     []byte
	Metadata map[string]any
}

func init() {
	for _, event := range claudeprofile.V140609ObservedExecutableEvents() {
		fact := event.Fact()
		for _, role := range event.EndpointRoles {
			registerExecutableEvents(role, map[string]string{fact: event.EventName})
		}
	}
}

func observedEventForSource(kind, source string) (claudeprofile.ObservedExecutableEvent, error) {
	event, ok := claudeprofile.V140609ObservedExecutableEvent(strings.TrimSpace(kind))
	if !ok || event.Source != source {
		return claudeprofile.ObservedExecutableEvent{}, invalidObserved("unsupported %s kind %q", source, kind)
	}
	return event, nil
}

func (m *Manager) RecordObservedRendererEvent(ctx context.Context, auth *cliproxyauth.Auth, observation ObservedRendererEvent) error {
	event, errEvent := observedEventForSource(observation.Kind, claudeprofile.ObservedEventSourceRenderer)
	if errEvent != nil {
		return errEvent
	}
	if errFields := validateObservedEventPayload(event, observation.Properties); errFields != nil {
		return errFields
	}
	segment, desktop := m.rendererWorkers(auth)
	identityWorker := desktop
	if identityWorker == nil {
		identityWorker = segment
	}
	if identityWorker == nil {
		return nil
	}
	identity := m.rendererSessionIdentity(identityWorker.binding, auth)
	encoded, errProperties := projectObservedRendererProperties(event, identity, observation.Properties)
	if errProperties != nil {
		return errProperties
	}
	facts := RequestFacts{
		SessionID: strings.TrimSpace(observation.SessionID), PromptID: strings.TrimSpace(observation.PromptID),
		ClientRequestID: strings.TrimSpace(observation.ClientRequestID),
	}
	if facts.SessionID == "" {
		facts.SessionID = m.appSessionID
	}
	if facts.ClientRequestID == "" {
		facts.ClientRequestID = uuid.NewString()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	hasSegment, hasDesktop := observedEventHasRole(event, segmentRole), observedEventHasRole(event, m.profile.EndpointRole)
	path := rendererCopyPathShell
	if hasDesktop {
		var errPath error
		path, errPath = observedRendererCopyPath(event, observation.Route, observation.SessionID)
		if errPath != nil {
			return errPath
		}
	}
	if hasSegment {
		kind := rendererCallTrack
		if event.RendererCall == claudeprofile.ObservedRendererCallPage {
			kind = rendererCallPage
		}
		if !hasDesktop {
			desktop = nil
		}
		m.emitRendererCallAt(ctx, kind, segment, desktop, facts, event.Fact(), encoded, path)
		return nil
	}
	if hasDesktop {
		return m.enqueueObservedDesktopRenderer(ctx, desktop, facts, event, encoded)
	}
	return fmt.Errorf("Claude Desktop renderer observation %q has no renderer endpoint", event.Kind)
}

const (
	observedRendererRouteNew     = "new"
	observedRendererRouteShell   = "shell"
	observedRendererRouteSession = "session"
)

func observedRendererCopyPath(event claudeprofile.ObservedExecutableEvent, route, sessionID string) (string, error) {
	var path string
	switch route = strings.TrimSpace(route); route {
	case "":
		path = rendererCopyPathShell
		if strings.TrimSpace(sessionID) != "" {
			path = rendererCopyPathSession
		}
	case observedRendererRouteNew:
		path = rendererCopyPathNew
	case observedRendererRouteShell:
		path = rendererCopyPathShell
	case observedRendererRouteSession:
		if strings.TrimSpace(sessionID) == "" {
			return "", invalidObserved("renderer kind %q route %q requires session_id", event.Kind, route)
		}
		path = rendererCopyPathSession
	default:
		return "", invalidObserved("renderer kind %q has unsupported route %q", event.Kind, route)
	}

	resolution, ok, errContract := claudeprofile.V140609ObservedEventPayloadContract(event.Kind)
	if errContract != nil {
		return "", errContract
	}
	if !ok {
		return "", fmt.Errorf("Claude Desktop observed event contract %q is unavailable", event.Kind)
	}
	if field, hasPath := resolution.Contract.Fields["path"]; hasPath {
		if errPath := validateObservedContractValue(event, "properties.path", path, field); errPath != nil {
			return "", errPath
		}
	}
	return path, nil
}

type observedLoginPageProperties struct {
	Path              string `json:"path"`
	Referrer          string `json:"referrer"`
	Search            string `json:"search"`
	URL               string `json:"url"`
	Surface           string `json:"surface"`
	DeploymentMode    string `json:"deployment_mode"`
	AppVersion        string `json:"app_version"`
	Name              string `json:"name"`
	CanonicalPath     string `json:"canonical_path"`
	CanonicalURL      string `json:"canonical_url"`
	CanonicalReferrer string `json:"canonical_referrer"`
}

type observedUpgradePageProperties struct {
	Path             string `json:"path"`
	Referrer         string `json:"referrer"`
	Search           string `json:"search"`
	URL              string `json:"url"`
	AccountUUID      string `json:"account_uuid"`
	OrganizationUUID string `json:"organization_uuid"`
	BillingType      string `json:"billing_type"`
	Surface          string `json:"surface"`
	DeploymentMode   string `json:"deployment_mode"`
	AppVersion       string `json:"app_version"`
	Name             string `json:"name"`
	CanonicalPath    string `json:"canonical_path"`
	CanonicalURL     string `json:"canonical_url"`
}

func projectObservedRendererProperties(event claudeprofile.ObservedExecutableEvent, identity rendererSessionIdentity, supplied map[string]any) (json.RawMessage, error) {
	if event.RendererCall == claudeprofile.ObservedRendererCallPage {
		referrer, _ := supplied["referrer"].(string)
		search, _ := supplied["search"].(string)
		canonicalURL := "https://claude.ai" + event.EventName
		pageURL := canonicalURL + search
		if event.Kind == "login" {
			return json.Marshal(observedLoginPageProperties{
				Path: event.EventName, Referrer: referrer, Search: search, URL: pageURL,
				Surface: rendererSessionSurface, DeploymentMode: rendererSessionDeployment, AppVersion: identity.AppVersion,
				Name: event.EventName, CanonicalPath: event.EventName, CanonicalURL: canonicalURL, CanonicalReferrer: referrer,
			})
		}
		return json.Marshal(observedUpgradePageProperties{
			Path: event.EventName, Referrer: referrer, Search: search, URL: pageURL,
			AccountUUID: identity.AccountUUID, OrganizationUUID: identity.OrganizationUUID, BillingType: identity.BillingType,
			Surface: rendererSessionSurface, DeploymentMode: rendererSessionDeployment, AppVersion: identity.AppVersion,
			Name: event.EventName, CanonicalPath: event.EventName, CanonicalURL: canonicalURL,
		})
	}
	properties := cloneObservedFields(supplied)
	for key, value := range map[string]any{
		"account_uuid": identity.AccountUUID, "organization_uuid": identity.OrganizationUUID,
		"billing_type": identity.BillingType, "surface": rendererSessionSurface,
		"deployment_mode": rendererSessionDeployment, "app_version": identity.AppVersion,
	} {
		if _, exists := properties[key]; exists {
			return nil, invalidObserved("renderer kind %q cannot override %q", event.Kind, key)
		}
		properties[key] = value
	}
	if _, exists := properties["version"]; !exists {
		properties["version"] = 1
	}
	resolution, okContract, errContract := claudeprofile.V140609ObservedEventPayloadContract(event.Kind)
	if errContract != nil {
		return nil, errContract
	}
	if okContract && len(resolution.Contract.FieldOrder) > 0 {
		return marshalObservedObjectInOrder(properties, resolution.Contract.FieldOrder)
	}
	return json.Marshal(properties)
}

func marshalObservedObjectInOrder(values map[string]any, order []string) (json.RawMessage, error) {
	if len(order) == 0 {
		return json.Marshal(values)
	}
	seen := make(map[string]struct{}, len(values))
	keys := make([]string, 0, len(values))
	for _, name := range order {
		if _, exists := values[name]; !exists {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		keys = append(keys, name)
	}
	var extras []string
	for name := range values {
		if _, ordered := seen[name]; !ordered {
			extras = append(extras, name)
		}
	}
	sort.Strings(extras)
	keys = append(keys, extras...)

	var encoded bytes.Buffer
	encoded.WriteByte('{')
	for index, name := range keys {
		if index > 0 {
			encoded.WriteByte(',')
		}
		key, errKey := json.Marshal(name)
		if errKey != nil {
			return nil, errKey
		}
		value, errValue := json.Marshal(values[name])
		if errValue != nil {
			return nil, errValue
		}
		encoded.Write(key)
		encoded.WriteByte(':')
		encoded.Write(value)
	}
	encoded.WriteByte('}')
	return append(json.RawMessage(nil), encoded.Bytes()...), nil
}

func (m *Manager) enqueueObservedDesktopRenderer(ctx context.Context, worker *accountWorker, facts RequestFacts, event claudeprofile.ObservedExecutableEvent, properties json.RawMessage) error {
	if worker == nil {
		return nil
	}
	profile, mapped := m.profile.Events[event.Fact()]
	if !mapped || profile.EventName != event.EventName {
		return fmt.Errorf("Claude Desktop renderer observation %q is not mapped", event.Kind)
	}
	eventID := uuid.NewString()
	payload, errMarshal := json.Marshal(desktopRendererEvent{
		EventType: desktopRendererEventType,
		EventData: desktopRendererEventData{
			EventName: profile.EventName, EventID: eventID, EventTimestamp: m.now().UTC().Format("2006-01-02T15:04:05.000Z"),
			AccountUUID: worker.binding.AccountUUID, OrganizationUUID: worker.binding.OrganizationUUID, Properties: string(properties),
		},
	})
	if errMarshal != nil {
		return errMarshal
	}
	return worker.enqueue(Envelope{
		Version: 1, EventUUID: eventID, EndpointRole: m.profile.EndpointRole, CatalogFact: event.Fact(), CatalogEvent: event.EventName,
		OccurredAt: m.now().UTC().Format(time.RFC3339Nano), Binding: worker.binding, SessionID: facts.SessionID,
		ClientRequestID: facts.ClientRequestID, Payload: payload,
	})
}

func (m *Manager) RecordObservedMainProcessEvent(ctx context.Context, auth *cliproxyauth.Auth, observation ObservedMainProcessEvent) error {
	event, errEvent := observedEventForSource(observation.Kind, claudeprofile.ObservedEventSourceMainProcess)
	if errEvent != nil {
		return errEvent
	}
	if errFields := validateObservedEventPayload(event, observation.Metadata); errFields != nil {
		return errFields
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	worker, errWorker := m.workerFor(auth)
	if errWorker != nil {
		return errWorker
	}
	if errEnqueue := worker.enqueueProjected(ctx, event.Fact(), strings.TrimSpace(observation.SessionID), strings.TrimSpace(observation.ClientRequestID), cloneObservedFields(observation.Metadata)); errEnqueue != nil {
		worker.recordQueueFailure(errEnqueue)
		return errEnqueue
	}
	return nil
}

func (m *Manager) RecordObservedSDKEvent(ctx context.Context, auth *cliproxyauth.Auth, observation ObservedSDKEvent) error {
	event, errEvent := observedEventForSource(observation.Kind, claudeprofile.ObservedEventSourceSDK)
	if errEvent != nil {
		return errEvent
	}
	eventFields := make(map[string]any)
	if skillName := strings.TrimSpace(observation.SkillName); skillName != "" {
		eventFields["skill_name"] = skillName
	}
	if errFields := validateObservedEventPayloadAndEventFields(event, observation.Metadata, eventFields); errFields != nil {
		return errFields
	}
	metadata := cloneObservedFields(observation.Metadata)
	if strings.TrimSpace(observation.SessionID) == "" || strings.TrimSpace(observation.Model) == "" {
		return invalidObserved("SDK kind %q requires session_id and model", event.Kind)
	}
	return m.recordSDKFact(ctx, auth, observation.SessionID, observation.Model, observation.PromptID, event.Fact(), func(subscription, promptID string) any {
		result := cloneObservedFields(metadata)
		if subscription != "" {
			result["subscription_type"] = subscription
		}
		if promptID != "" {
			result["cc_prompt_id"] = promptID
		}
		return observedSDKMetadata{
			values: result, skillName: strings.TrimSpace(observation.SkillName),
			includeSampleRate: event.Kind == "tengu_plugin_name_collision",
		}
	})
}

type observedSDKMetadata struct {
	values            map[string]any
	skillName         string
	includeSampleRate bool
}

func (m observedSDKMetadata) MarshalJSON() ([]byte, error) {
	values := cloneObservedFields(m.values)
	if m.includeSampleRate {
		values["sample_rate"] = 0.01
	}
	return json.Marshal(values)
}

func (m observedSDKMetadata) sdkEventSkillName() *string {
	if m.skillName == "" {
		return nil
	}
	value := m.skillName
	return &value
}

func (m *Manager) RecordObservedPerformanceEvent(ctx context.Context, auth *cliproxyauth.Auth, observation ObservedPerformanceEvent) error {
	event, errEvent := observedEventForSource(observation.Kind, claudeprofile.ObservedEventSourcePerformance)
	if errEvent != nil {
		return errEvent
	}
	if errFields := validateObservedEventPayload(event, observation.Data); errFields != nil {
		return errFields
	}
	eventBody, ok := observation.Data[event.EventName].(map[string]any)
	if !ok || len(eventBody) == 0 {
		return invalidObserved("performance kind %q requires a %q object", event.Kind, event.EventName)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	delivery, okDelivery := m.auxiliaryDeliveries[datadogRUMRole]
	if !okDelivery {
		return fmt.Errorf("Datadog RUM delivery profile is unavailable")
	}
	worker, errWorker := m.workerForDelivery(auth, delivery)
	if errWorker != nil {
		return errWorker
	}
	payload, errPayload := m.projectObservedRUM(worker, event, observation)
	if errPayload != nil {
		return errPayload
	}
	sessionID := strings.TrimSpace(observation.SessionID)
	if sessionID == "" {
		sessionID = m.appSessionID
	}
	clientRequestID := strings.TrimSpace(observation.ClientRequestID)
	if clientRequestID == "" {
		clientRequestID = uuid.NewString()
	}
	if errEnqueue := worker.enqueue(Envelope{
		Version: 1, EventUUID: uuid.NewString(), EndpointRole: datadogRUMRole, CatalogFact: event.Fact(), CatalogEvent: event.EventName,
		OccurredAt: m.now().UTC().Format(time.RFC3339Nano), Binding: worker.binding, SessionID: sessionID,
		ClientRequestID: clientRequestID, Payload: payload,
	}); errEnqueue != nil {
		worker.recordQueueFailure(errEnqueue)
		return errEnqueue
	}
	return nil
}

func (m *Manager) projectObservedRUM(worker *accountWorker, event claudeprofile.ObservedExecutableEvent, observation ObservedPerformanceEvent) ([]byte, error) {
	materials, errMaterials := runtimeMaterialsForDelivery(worker.authSnapshot(), worker.profile)
	if errMaterials != nil {
		return nil, errMaterials
	}
	data := cloneObservedFields(observation.Data)
	if event.EventName == auxiliaryRUMTelemetryName {
		return m.projectObservedRUMTelemetry(worker, materials, data)
	}
	ddInput, _ := data["_dd"].(map[string]any)
	dd := observedRUMDD{
		FormatVersion: 2,
		Drift:         ddInput["drift"],
		Configuration: observedRUMDDConfiguration{
			SessionSampleRate: 100, SessionReplaySampleRate: 0, ProfilingSampleRate: 0, TraceSampleRate: 100,
		},
		SDKName: "rum", Discarded: false,
	}
	dd.TraceID, _ = ddInput["trace_id"].(string)
	dd.SpanID, _ = ddInput["span_id"].(string)
	payload := observedRUMPerformancePayload{
		Type: event.EventName, DD: dd,
		Application: observedRUMApplication{ID: materials["application_id"]},
		Date:        m.now().UTC().UnixMilli(), Source: "browser",
		View: observedRUMViewFrom(data["view"]),
		Session: observedRUMSession{
			ID: deterministicUUID("rum-session", worker.binding.RuntimeUUID, m.appSessionID), Type: "user",
		},
		Connectivity: data["connectivity"], Tab: data["tab"], Context: data["context"],
		USR: observedRUMUser{
			ID: worker.binding.AccountUUID, OrganizationID: worker.binding.OrganizationUUID,
			Plan: subscriptionType(worker.authSnapshot()), AnonymousID: worker.binding.RuntimeUUID,
		},
		Display: data["display"], Service: "claude-ai",
		Version: stringValue(data["version"]), DDTags: stringValue(data["ddtags"]),
	}
	if event.EventName == auxiliaryRUMResourceName {
		payload.Resource = data[auxiliaryRUMResourceName]
	} else {
		payload.LongTask = data[auxiliaryRUMLongTaskName]
	}
	return json.Marshal(payload)
}

type observedRUMApplication struct {
	ID string `json:"id"`
}

type observedRUMSession struct {
	ID   string `json:"id"`
	Type string `json:"type,omitempty"`
}

type observedRUMUser struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organization_id"`
	Plan           string `json:"plan"`
	AnonymousID    string `json:"anonymous_id"`
}

type observedRUMDDConfiguration struct {
	SessionSampleRate       int `json:"session_sample_rate"`
	SessionReplaySampleRate int `json:"session_replay_sample_rate"`
	ProfilingSampleRate     int `json:"profiling_sample_rate"`
	TraceSampleRate         int `json:"trace_sample_rate"`
}

type observedRUMDD struct {
	FormatVersion int                        `json:"format_version"`
	Drift         any                        `json:"drift"`
	Configuration observedRUMDDConfiguration `json:"configuration"`
	SDKName       string                     `json:"sdk_name"`
	Discarded     bool                       `json:"discarded"`
	TraceID       string                     `json:"trace_id,omitempty"`
	SpanID        string                     `json:"span_id,omitempty"`
}

type observedRUMTelemetryDD struct {
	FormatVersion int `json:"format_version"`
}

type observedRUMView struct {
	URL      string `json:"url"`
	Referrer string `json:"referrer"`
	ID       string `json:"id"`
	Name     string `json:"name"`
}

func observedRUMViewFrom(value any) observedRUMView {
	fields, _ := value.(map[string]any)
	return observedRUMView{
		URL: stringValue(fields["url"]), Referrer: stringValue(fields["referrer"]),
		ID: stringValue(fields["id"]), Name: stringValue(fields["name"]),
	}
}

type observedRUMPerformancePayload struct {
	Type         string                 `json:"type"`
	DD           observedRUMDD          `json:"_dd"`
	Application  observedRUMApplication `json:"application"`
	Date         int64                  `json:"date"`
	Source       string                 `json:"source"`
	View         observedRUMView        `json:"view"`
	Session      observedRUMSession     `json:"session"`
	Connectivity any                    `json:"connectivity"`
	Tab          any                    `json:"tab"`
	Context      any                    `json:"context"`
	USR          observedRUMUser        `json:"usr"`
	Display      any                    `json:"display"`
	Service      string                 `json:"service"`
	Version      string                 `json:"version"`
	Resource     any                    `json:"resource,omitempty"`
	LongTask     any                    `json:"long_task,omitempty"`
	DDTags       string                 `json:"ddtags"`
}

type observedRUMTelemetryPayload struct {
	Type                 string                  `json:"type"`
	Date                 int64                   `json:"date"`
	Service              string                  `json:"service"`
	Version              string                  `json:"version"`
	Source               string                  `json:"source"`
	DD                   observedRUMTelemetryDD  `json:"_dd"`
	Telemetry            any                     `json:"telemetry"`
	DDTags               string                  `json:"ddtags"`
	ExperimentalFeatures []any                   `json:"experimental_features"`
	Session              *observedRUMSession     `json:"session,omitempty"`
	AnonymousID          string                  `json:"anonymous_id,omitempty"`
	Application          *observedRUMApplication `json:"application,omitempty"`
	Action               any                     `json:"action,omitempty"`
	View                 any                     `json:"view,omitempty"`
}

func (m *Manager) projectObservedRUMTelemetry(worker *accountWorker, materials map[string]string, data map[string]any) ([]byte, error) {
	telemetryFields, _ := data["telemetry"].(map[string]any)
	payload := observedRUMTelemetryPayload{
		Type: "telemetry", Date: m.now().UTC().UnixMilli(), Service: "browser-rum-sdk", Version: auxiliaryRUMSDKVersion,
		Source: "browser", DD: observedRUMTelemetryDD{FormatVersion: 2}, Telemetry: telemetryFields,
		DDTags: stringValue(data["ddtags"]), ExperimentalFeatures: []any{},
	}
	typeName := stringValue(telemetryFields["type"])
	_, hasAction := data["action"]
	_, hasView := data["view"]
	if typeName == "configuration" || hasAction || hasView {
		payload.Session = &observedRUMSession{ID: deterministicUUID("rum-session", worker.binding.RuntimeUUID, m.appSessionID)}
		payload.Application = &observedRUMApplication{ID: materials["application_id"]}
		payload.AnonymousID = stringValue(data["anonymous_id"])
	}
	if typeName == "usage" && hasAction && hasView {
		payload.Action = data["action"]
		payload.View = data["view"]
	}
	return json.Marshal(payload)
}

func (m *Manager) RecordObservedCrashAttachment(ctx context.Context, auth *cliproxyauth.Auth, observation ObservedCrashAttachment) error {
	event, errEvent := observedEventForSource("attachment", claudeprofile.ObservedEventSourceCrash)
	if errEvent != nil {
		return errEvent
	}
	if errFields := validateObservedEventFields(observation.Metadata); errFields != nil {
		return errFields
	}
	resolution, okContract, errContract := claudeprofile.V140609ObservedEventPayloadContract(event.Kind)
	if errContract != nil {
		return errContract
	}
	if !okContract || resolution.Contract.Binary == nil {
		return fmt.Errorf("Claude Desktop crash attachment contract is unavailable")
	}
	binary := resolution.Contract.Binary
	filename := strings.TrimSpace(observation.Filename)
	filenameUUID := strings.TrimSuffix(filename, ".dmp")
	parsedFilenameUUID, errFilenameUUID := uuid.Parse(filenameUUID)
	if filename == "" || filepath.Base(filename) != filename || filepath.Ext(filename) != ".dmp" ||
		errFilenameUUID != nil || parsedFilenameUUID.String() != filenameUUID {
		return invalidObserved("crash filename must be a canonical UUID with a .dmp suffix")
	}
	minimumBytes, maximumBytes := binary.MinimumBytes, binary.MaximumBytes
	if maximumBytes <= 0 {
		maximumBytes = maxObservedCrashAttachmentBytes
	}
	if len(observation.Data) < minimumBytes || len(observation.Data) > maximumBytes {
		return invalidObserved("crash attachment size must be between %d and %d bytes", minimumBytes, maximumBytes)
	}
	if binary.MagicASCII == "" || !bytes.HasPrefix(observation.Data, []byte(binary.MagicASCII)) {
		return invalidObserved("crash attachment does not have the captured %s magic", binary.MagicASCII)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	delivery, okDelivery := m.auxiliaryDeliveries[sentryRole]
	if !okDelivery {
		return fmt.Errorf("Sentry delivery profile is unavailable")
	}
	worker, errWorker := m.workerForDelivery(auth, delivery)
	if errWorker != nil {
		return errWorker
	}
	eventID := uuid.NewString()
	crashEvent := map[string]any{
		"event_id": strings.ReplaceAll(eventID, "-", ""), "timestamp": float64(m.now().UTC().UnixNano()) / float64(time.Second),
		"platform": "javascript", "level": "fatal", "environment": "production", "release": "Claude@" + strings.TrimSuffix(m.bundle.DesktopVersion, ".0"),
		"user":      map[string]any{"id": worker.binding.AccountUUID},
		"tags":      map[string]any{"deployment_mode": "1p", "renderer_surface": "epitaxy", "crash_source": "desktop_companion"},
		"contexts":  map[string]any{"app": map[string]any{"app_identifier": m.profile.Runtime.ClientApp, "app_version": m.profile.Runtime.ClientVersion}},
		"exception": map[string]any{"values": []map[string]any{{"type": "NativeCrash", "value": "Claude Desktop native process crash", "mechanism": map[string]any{"type": "minidump", "handled": false}}}},
		"extra":     cloneObservedFields(observation.Metadata),
	}
	eventPayload, errMarshal := json.Marshal(crashEvent)
	if errMarshal != nil {
		return errMarshal
	}
	if errEnqueue := worker.enqueue(Envelope{
		Version: 1, EventUUID: eventID, EndpointRole: sentryRole, CatalogFact: FactRequestFailed, CatalogEvent: "event",
		OccurredAt: m.now().UTC().Format(time.RFC3339Nano), Binding: worker.binding, SessionID: m.appSessionID, Payload: eventPayload,
	}); errEnqueue != nil {
		worker.recordQueueFailure(errEnqueue)
		return errEnqueue
	}
	encodedAttachment, _ := json.Marshal(base64.StdEncoding.EncodeToString(observation.Data))
	if errEnqueue := worker.enqueue(Envelope{
		Version: 1, EventUUID: uuid.NewString(), EndpointRole: sentryRole, CatalogFact: event.Fact(), CatalogEvent: event.EventName,
		OccurredAt: m.now().UTC().Format(time.RFC3339Nano), Binding: worker.binding, SessionID: m.appSessionID,
		Payload: encodedAttachment, PayloadEncoding: "base64", ItemHeaders: map[string]any{
			"filename": filename, "attachment_type": "event.minidump",
		},
	}); errEnqueue != nil {
		worker.recordQueueFailure(errEnqueue)
		return errEnqueue
	}
	return nil
}

func observedEventHasRole(event claudeprofile.ObservedExecutableEvent, role string) bool {
	for _, candidate := range event.EndpointRoles {
		if candidate == role {
			return true
		}
	}
	return false
}

func cloneObservedFields(values map[string]any) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func validateObservedEventFields(values map[string]any) error {
	if errFields := validateObservedEventJSON(values); errFields != nil {
		return errFields
	}
	if observedFieldsContainSecret(values, 0) {
		return invalidObserved("fields contain a credential-bearing key")
	}
	return nil
}

func validateObservedEventJSON(values map[string]any) error {
	encoded, errMarshal := json.Marshal(values)
	if errMarshal != nil {
		return invalidObserved("fields are not JSON-compatible: %v", errMarshal)
	}
	if len(encoded) > maxObservedEventFieldsBytes {
		return invalidObserved("fields exceed %d bytes", maxObservedEventFieldsBytes)
	}
	return nil
}

func validateObservedEventPayload(event claudeprofile.ObservedExecutableEvent, values map[string]any) error {
	return validateObservedEventPayloadAndEventFields(event, values, nil)
}

func validateObservedEventPayloadAndEventFields(event claudeprofile.ObservedExecutableEvent, values, eventFields map[string]any) error {
	if errFields := validateObservedEventJSON(values); errFields != nil {
		return errFields
	}
	if errFields := validateObservedEventJSON(eventFields); errFields != nil {
		return errFields
	}
	if observedEventFieldsContainSecret(event, values) {
		return invalidObserved("fields contain a credential-bearing key")
	}
	if observedFieldsContainSecret(eventFields, 0) {
		return invalidObserved("event fields contain a credential-bearing key")
	}
	for key := range values {
		if observedProgramOwnedField(event.Source, key) {
			return invalidObserved("%s kind %q cannot override %q", event.Source, event.Kind, key)
		}
	}
	resolution, hasContract, errContract := claudeprofile.V140609ObservedEventPayloadContract(event.Kind)
	if errContract != nil {
		return errContract
	}
	if !hasContract {
		return fmt.Errorf("Claude Desktop observed event contract %q is unavailable", event.Kind)
	}
	if resolution.Classification == claudeprofile.ObservedPayloadContractEndpointOnly {
		if len(eventFields) != 0 {
			return invalidObserved("%s kind %q has no observed event-field contract", event.Source, event.Kind)
		}
		return nil
	}
	contract := resolution.Contract
	if len(contract.Variants) > 0 {
		if errVariant := validateObservedObjectVariants(event, contract.PayloadField, values, contract.Variants, true); errVariant != nil {
			return errVariant
		}
	} else if errPayload := validateObservedObjectContract(event, contract.PayloadField, values, contract.Fields, contract.AdditionalProperties, true); errPayload != nil {
		return errPayload
	}
	if errEventFields := validateObservedObjectContract(event, "event_fields", eventFields, contract.EventFields, false, false); errEventFields != nil {
		return errEventFields
	}
	return nil
}

func validateObservedObjectVariants(event claudeprofile.ObservedExecutableEvent, path string, values map[string]any, variants []claudeprofile.ObservedEventObjectContract, topLevel bool) error {
	for _, variant := range variants {
		if validateObservedObjectContract(event, path, values, variant.Fields, variant.AdditionalProperties, topLevel) == nil {
			return nil
		}
	}
	return invalidObserved("%s kind %q %s does not match any observed payload variant", event.Source, event.Kind, path)
}

func validateObservedObjectContract(event claudeprofile.ObservedExecutableEvent, path string, values map[string]any, fields map[string]claudeprofile.ObservedEventFieldContract, additionalProperties, topLevel bool) error {
	for key, value := range values {
		field, ok := fields[key]
		if !ok {
			if additionalProperties {
				continue
			}
			return invalidObserved("%s kind %q %s has no observed field %q", event.Source, event.Kind, path, key)
		}
		if field.Owner == claudeprofile.ObservedFieldOwnerProgram || (topLevel && observedProgramOwnedField(event.Source, key)) {
			return invalidObserved("%s kind %q cannot override %q", event.Source, event.Kind, observedFieldPath(path, key))
		}
		if errValue := validateObservedContractValue(event, observedFieldPath(path, key), value, field); errValue != nil {
			return errValue
		}
	}
	for name, field := range fields {
		if !field.Required || field.Owner == claudeprofile.ObservedFieldOwnerProgram || (topLevel && observedProgramOwnedField(event.Source, name)) {
			continue
		}
		if _, exists := values[name]; !exists {
			return invalidObserved("%s kind %q requires observed field %q", event.Source, event.Kind, observedFieldPath(path, name))
		}
	}
	return nil
}

func validateObservedContractValue(event claudeprofile.ObservedExecutableEvent, path string, value any, field claudeprofile.ObservedEventFieldContract) error {
	actualType := observedValueType(value)
	if !observedFieldAllowsType(field.Types, actualType) {
		return invalidObserved("%s kind %q field %q has type %s, want %s", event.Source, event.Kind, path, actualType, strings.Join(field.Types, " or "))
	}
	if field.NonEmpty && observedValueEmpty(value) {
		return invalidObserved("%s kind %q field %q must be non-empty", event.Source, event.Kind, path)
	}
	if len(field.Enum) > 0 {
		stringValue, ok := value.(string)
		if !ok || !observedStringInSet(stringValue, field.Enum) {
			return invalidObserved("%s kind %q field %q is outside the observed enum", event.Source, event.Kind, path)
		}
	}
	if len(field.Const) > 0 && !observedJSONConstMatches(value, field.Const) {
		return invalidObserved("%s kind %q field %q does not match the observed constant", event.Source, event.Kind, path)
	}
	if actualType == "object" && (field.Fields != nil || len(field.Variants) > 0 || field.AdditionalProperties) {
		object, ok := observedObjectValue(value)
		if !ok {
			return invalidObserved("%s kind %q field %q is not a JSON object", event.Source, event.Kind, path)
		}
		if len(field.Variants) > 0 {
			return validateObservedObjectVariants(event, path, object, field.Variants, false)
		}
		return validateObservedObjectContract(event, path, object, field.Fields, field.AdditionalProperties, false)
	}
	if actualType == "array" && field.Items != nil {
		items, ok := observedArrayValue(value)
		if !ok {
			return invalidObserved("%s kind %q field %q is not a JSON array", event.Source, event.Kind, path)
		}
		for index, item := range items {
			if errItem := validateObservedContractValue(event, fmt.Sprintf("%s[%d]", path, index), item, *field.Items); errItem != nil {
				return errItem
			}
		}
	}
	return nil
}

func observedFieldPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "." + name
}

func observedValueEmpty(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) == ""
	case map[string]any:
		return len(typed) == 0
	case []any:
		return len(typed) == 0
	}
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() {
		return true
	}
	switch reflected.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice:
		return reflected.Len() == 0
	}
	return false
}

func observedStringInSet(value string, allowed []string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func observedJSONConstMatches(value any, expected json.RawMessage) bool {
	actualJSON, errActual := json.Marshal(value)
	if errActual != nil {
		return false
	}
	var actualValue any
	actualDecoder := json.NewDecoder(bytes.NewReader(actualJSON))
	actualDecoder.UseNumber()
	if errDecode := actualDecoder.Decode(&actualValue); errDecode != nil {
		return false
	}
	var expectedValue any
	expectedDecoder := json.NewDecoder(bytes.NewReader(expected))
	expectedDecoder.UseNumber()
	if errDecode := expectedDecoder.Decode(&expectedValue); errDecode != nil {
		return false
	}
	return reflect.DeepEqual(actualValue, expectedValue)
}

func observedObjectValue(value any) (map[string]any, bool) {
	if object, ok := value.(map[string]any); ok {
		return object, true
	}
	encoded, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, false
	}
	var object map[string]any
	if errUnmarshal := json.Unmarshal(encoded, &object); errUnmarshal != nil || object == nil {
		return nil, false
	}
	return object, true
}

func observedArrayValue(value any) ([]any, bool) {
	if array, ok := value.([]any); ok {
		return array, true
	}
	encoded, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, false
	}
	var array []any
	if errUnmarshal := json.Unmarshal(encoded, &array); errUnmarshal != nil || array == nil {
		return nil, false
	}
	return array, true
}

func observedProgramOwnedField(source, name string) bool {
	switch source {
	case claudeprofile.ObservedEventSourceRenderer:
		switch name {
		case "account_uuid", "organization_uuid", "billing_type", "surface", "deployment_mode", "app_version", "version",
			"_dual_fire", "anonymous_id", "service_name", "path", "url", "name", "canonical_path", "canonical_url":
			return true
		}
	case claudeprofile.ObservedEventSourceMainProcess:
		switch name {
		case "arch", "backend_kind", "config_source", "config_source_remote", "deployment_mode", "desktop_variant",
			"inference_provider", "installer_variant", "is_ssh", "platform", "product_surface", "renderer_surface",
			"running_under_translation", "app_version", "commit_hash", "app_session_id", "app_uptime_seconds", "organization_id":
			return true
		}
	case claudeprofile.ObservedEventSourceSDK:
		return name == "subscription_type" || name == "cc_prompt_id"
	}
	return false
}

func observedValueType(value any) string {
	if value == nil {
		return "null"
	}
	switch value.(type) {
	case string:
		return "string"
	case bool:
		return "bool"
	case json.Number:
		return "number"
	}
	kind := reflect.ValueOf(value).Kind()
	switch kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Map, reflect.Struct:
		return "object"
	case reflect.Array, reflect.Slice:
		return "array"
	default:
		return kind.String()
	}
}

func observedFieldAllowsType(types []string, actual string) bool {
	for _, candidate := range types {
		switch candidate {
		case actual:
			return true
		case "boolean":
			if actual == "bool" {
				return true
			}
		case "integer":
			if actual == "number" {
				return true
			}
		case "json-string", "base64-json":
			if actual == "string" {
				return true
			}
		}
	}
	return false
}

func observedFieldsContainSecret(value any, depth int) bool {
	if depth > 16 {
		return true
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if observedCredentialFieldName(key) {
				return true
			}
			if observedFieldsContainSecret(child, depth+1) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if observedFieldsContainSecret(child, depth+1) {
				return true
			}
		}
	}
	return false
}

func observedEventFieldsContainSecret(event claudeprofile.ObservedExecutableEvent, values map[string]any) bool {
	for key, value := range values {
		if observedCredentialFieldName(key) {
			if event.Kind == "desktop_notification_reachability" && strings.EqualFold(strings.TrimSpace(key), "authorization") {
				permission, ok := value.(string)
				if ok && (permission == "default" || permission == "denied" || permission == "granted") {
					continue
				}
			}
			return true
		}
		if observedFieldsContainSecret(value, 1) {
			return true
		}
	}
	return false
}

func observedCredentialFieldName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "authorization", "cookie", "set-cookie", "access_token", "refresh_token", "api_key", "password", "secret":
		return true
	default:
		return false
	}
}
