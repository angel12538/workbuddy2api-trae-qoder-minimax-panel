package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"qoder2api/account"
	"qoder2api/internal/bridge"
)

func main() {
	root := "/src"
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	poolRaw, err := os.ReadFile(root + "/data/ide_accounts/qoder_accounts.json")
	if err != nil {
		panic(err)
	}
	var pool struct {
		Accounts []struct {
			Email string `json:"email"`
			Auth  struct {
				Token  string `json:"token"`
				Realm  string `json:"realm"`
			} `json:"auth"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(poolRaw, &pool); err != nil {
		panic(err)
	}
	curRaw, _ := os.ReadFile(root + "/data/ide_accounts/qoder_current.json")
	var cur struct {
		Key string `json:"account_key"`
	}
	_ = json.Unmarshal(curRaw, &cur)

	token, region := "", "global"
	for _, a := range pool.Accounts {
		if cur.Key != "" && !strings.EqualFold(cur.Key, a.Email) {
			continue
		}
		if a.Auth.Token == "" {
			continue
		}
		token = a.Auth.Token
		if strings.EqualFold(a.Auth.Realm, "cn") {
			region = "cn"
		}
		break
	}
	if token == "" {
		panic("no current qoder token")
	}

	b, err := bridge.NewBridgeWithFP(token, account.NormalizeRegion(region), map[string]interface{}{}, nil)
	if err != nil {
		panic(err)
	}
	raw, err := b.RawModelList()
	if err != nil {
		panic(err)
	}
	out, _ := json.MarshalIndent(raw, "", "  ")
	path := root + "/data/qoder_raw_models.json"
	if err := os.WriteFile(path, out, 0o644); err != nil {
		panic(err)
	}
	fmt.Println("wrote", path, len(out), "bytes")
}
