package claudedesktop

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

func extractBootstrapIdentity(raw []byte, organizationUUID string) (AccountIdentity, error) {
	var payload any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return AccountIdentity{}, errUnmarshal
	}
	identity := AccountIdentity{OrganizationUUID: strings.ToLower(strings.TrimSpace(organizationUUID))}
	var walk func(any, string)
	walk = func(value any, parent string) {
		switch typed := value.(type) {
		case map[string]any:
			lowerParent := strings.ToLower(parent)
			if lowerParent == "account" || lowerParent == "user" || lowerParent == "current_account" {
				if identity.AccountUUID == "" {
					identity.AccountUUID = firstMapString(typed, "uuid", "id", "account_uuid", "accountUuid")
				}
				if identity.Email == "" {
					identity.Email = firstMapString(typed, "email", "email_address", "emailAddress")
				}
			}
			candidateOrg := firstMapString(typed, "uuid", "id", "organization_uuid", "organizationUuid")
			if (lowerParent == "organization" || lowerParent == "active_organization" || lowerParent == "current_organization") &&
				(identity.OrganizationUUID == "" || strings.EqualFold(candidateOrg, identity.OrganizationUUID)) {
				if identity.OrganizationUUID == "" {
					identity.OrganizationUUID = candidateOrg
				}
				if identity.OrganizationName == "" {
					identity.OrganizationName = firstMapString(typed, "name", "display_name", "displayName")
				}
			}
			if identity.AccountUUID == "" {
				identity.AccountUUID = firstMapString(typed, "account_uuid", "accountUuid")
			}
			if identity.Email == "" {
				identity.Email = firstMapString(typed, "email", "email_address", "emailAddress")
			}
			for key, child := range typed {
				walk(child, key)
			}
		case []any:
			for _, child := range typed {
				walk(child, parent)
			}
		}
	}
	walk(payload, "")
	identity = normalizeIdentity(identity)
	if _, errAccount := uuid.Parse(identity.AccountUUID); errAccount != nil {
		return AccountIdentity{}, fmt.Errorf("Claude account bootstrap contained no valid account UUID")
	}
	if strings.TrimSpace(identity.Email) == "" {
		return AccountIdentity{}, fmt.Errorf("Claude account bootstrap contained no account email")
	}
	return identity, nil
}

func firstMapString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
