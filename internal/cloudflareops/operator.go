package cloudflareops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

const operatorTokenBinding = "OPERATOR_TOKEN_SHA256"

// RegisterOperatorToken uploads a digest because the binding must not contain the raw Cloudflare credential.
func (client *Client) RegisterOperatorToken(ctx context.Context, account, script string, token []byte) error {
	if account == "" || script == "" || strings.ContainsAny(account+script, "/?#") || len(token) == 0 {
		return errors.New("operator registration requires account, script, and token")
	}
	digest := sha256.Sum256(token)
	body, err := json.Marshal(struct {
		Name string `json:"name"`
		Text string `json:"text"`
		Type string `json:"type"`
	}{Name: operatorTokenBinding, Text: hex.EncodeToString(digest[:]), Type: "secret_text"})
	if err != nil {
		return errors.New("operator digest could not be encoded")
	}
	response, _, err := client.request(ctx, http.MethodPut, "/accounts/"+account+"/workers/scripts/"+script+"/secrets", body)
	if err != nil {
		return err
	}
	var binding struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if json.Unmarshal(response.Result, &binding) != nil || binding.Name != operatorTokenBinding || binding.Type != "secret_text" {
		return errors.New("cloudflare did not acknowledge the operator binding")
	}
	return nil
}
