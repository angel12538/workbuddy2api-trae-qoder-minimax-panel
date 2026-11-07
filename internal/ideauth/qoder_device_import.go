package ideauth

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// QoderIdentityKey keeps the legacy email routing contract, while accounts
// whose verified profile has no email get an explicit UID key, never a fake email.
func QoderIdentityKey(auth *QoderAuth) string {
	if auth == nil {
		return ""
	}
	if key := strings.TrimSpace(auth.AccountKey); key != "" {
		return key
	}
	if email := strings.TrimSpace(auth.User.Email); email != "" {
		return email
	}
	if uid := strings.TrimSpace(auth.User.ID); uid != "" {
		if auth.Realm != "" {
			return "qoder:" + auth.Realm + ":" + uid
		}
		return "qoder:" + uid
	}
	return ""
}

// ImportQoderDeviceAuth merges a verified device grant under the same lock as
// all other pool writers. Preserve unknown pool, account and auth fields.
func ImportQoderDeviceAuth(auth *QoderAuth, machine string) error {
	if auth == nil || auth.Token == "" || strings.TrimSpace(auth.User.ID) == "" {
		return fmt.Errorf("incomplete identity")
	}
	qoderPoolMu.Lock()
	defer qoderPoolMu.Unlock()
	root := map[string]json.RawMessage{}
	raw, err := os.ReadFile(qoderPoolPath())
	if err == nil {
		if err := json.Unmarshal(raw, &root); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if root == nil {
		return fmt.Errorf("invalid pool")
	}
	var rows []map[string]json.RawMessage
	if b, ok := root["accounts"]; ok {
		if err := json.Unmarshal(b, &rows); err != nil {
			return err
		}
	}
	var target, legacy map[string]json.RawMessage
	for _, row := range rows {
		if row == nil {
			return fmt.Errorf("null account entry")
		}
		var email, uid string
		_ = json.Unmarshal(row["email"], &email)
		_ = json.Unmarshal(row["user_id"], &uid)
		var saved QoderAuth
		if b := row["auth"]; len(b) > 0 && string(b) != "null" {
			if err := json.Unmarshal(b, &saved); err != nil {
				return err
			}
		}
		if uid == "" {
			uid = saved.User.ID
		}
		sameRealm := saved.Realm == "" || auth.Realm == "" || saved.Realm == auth.Realm
		if uid == auth.User.ID && sameRealm {
			target = row
			break
		}
		if auth.User.Email != "" && strings.EqualFold(email, auth.User.Email) && uid == "" && sameRealm {
			legacy = row
		}
	}
	if target == nil {
		target = legacy
	}
	if target == nil {
		target = map[string]json.RawMessage{}
		rows = append(rows, target)
	}
	var existingKey string
	_ = json.Unmarshal(target["email"], &existingKey)
	if existingKey != "" {
		auth.AccountKey = existingKey
	} else {
		auth.AccountKey = ""
		key := QoderIdentityKey(auth)
		for _, row := range rows {
			var savedKey string
			_ = json.Unmarshal(row["email"], &savedKey)
			if strings.EqualFold(savedKey, key) {
				key = "qoder:" + auth.Realm + ":" + auth.User.ID
				break
			}
		}
		auth.AccountKey = key
	}
	set := func(k string, v any) { target[k], _ = json.Marshal(v) }
	set("email", auth.AccountKey)
	set("user_id", auth.User.ID)
	set("saved_at", time.Now().Format(time.RFC3339))
	if len(target["machine_id"]) == 0 || string(target["machine_id"]) == `""` {
		set("machine_id", machine)
	}
	oldAuth := map[string]json.RawMessage{}
	if b := target["auth"]; len(b) > 0 && string(b) != "null" {
		if err := json.Unmarshal(b, &oldAuth); err != nil {
			return err
		}
	}
	if oldAuth == nil {
		oldAuth = map[string]json.RawMessage{}
	}
	// The same UID may have a previously known email which userinfo omits.
	var savedUser QoderUser
	if raw := oldAuth["user"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &savedUser); err != nil {
			return err
		}
	}
	if auth.User.Email == "" {
		auth.User.Email = savedUser.Email
	}
	if auth.User.Name == "" {
		auth.User.Name = savedUser.Name
	}
	if auth.User.Name == "" {
		_ = json.Unmarshal(target["name"], &auth.User.Name)
	}
	if auth.User.AvatarURL == "" {
		auth.User.AvatarURL = savedUser.AvatarURL
	}
	if auth.User.Phone == nil {
		auth.User.Phone = savedUser.Phone
	}
	set("name", auth.User.Name)
	b, err := json.Marshal(auth)
	if err != nil {
		return err
	}
	var newAuth map[string]json.RawMessage
	if err := json.Unmarshal(b, &newAuth); err != nil {
		return err
	}
	oldUser := map[string]json.RawMessage{}
	if raw := oldAuth["user"]; len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &oldUser); err != nil {
			return err
		}
	}
	if oldUser == nil {
		oldUser = map[string]json.RawMessage{}
	}
	var newUser map[string]json.RawMessage
	if err := json.Unmarshal(newAuth["user"], &newUser); err != nil {
		return err
	}
	for k, v := range newUser {
		oldUser[k] = v
	}
	newAuth["user"], err = json.Marshal(oldUser)
	if err != nil {
		return err
	}
	for k, v := range newAuth {
		oldAuth[k] = v
	}
	set("auth", oldAuth)
	root["accounts"], err = json.Marshal(rows)
	if err != nil {
		return err
	}
	b, err = json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(qoderPoolPath(), b)
}
