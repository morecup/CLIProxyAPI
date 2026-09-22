package telemetry

import "strings"

const FactSDKSkillLoaded = "skill_loaded"

const sdkStartupOrderSkillLoaded = 410

type sdkSkillLoadedMetadata struct {
	SubscriptionType string `json:"subscription_type,omitempty"`
	name             string
}

func (m sdkSkillLoadedMetadata) sdkEventSkillName() *string {
	name := m.name
	return &name
}

func init() {
	registerExecutableEvents("sdk-event-logging", map[string]string{
		FactSDKSkillLoaded: "tengu_skill_loaded",
	})
	registerSDKStartupEvent(sdkStartupEvent{
		Fact:  FactSDKSkillLoaded,
		Order: sdkStartupOrderSkillLoaded,
		BuildMany: func(m *Manager, subscription string) []any {
			if m == nil || m.bundle == nil {
				return nil
			}
			seen := make(map[string]bool, len(m.bundle.ControlPlane.Worker.Skills))
			result := make([]any, 0, len(m.bundle.ControlPlane.Worker.Skills))
			for _, raw := range m.bundle.ControlPlane.Worker.Skills {
				name := strings.TrimSpace(raw)
				if name == "" || seen[name] {
					continue
				}
				seen[name] = true
				result = append(result, sdkSkillLoadedMetadata{SubscriptionType: subscription, name: name})
			}
			return result
		},
	})
}
