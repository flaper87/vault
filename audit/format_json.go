package audit

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/hashicorp/vault/logical"
)

// FormatJSON is a Formatter implementation that structuteres data into
// a JSON format.
type FormatJSON struct{}

func (f *FormatJSON) FormatRequest(
	w io.Writer,
	auth *logical.Auth, req *logical.Request) error {
	// If auth is nil, make an empty one
	if auth == nil {
		auth = new(logical.Auth)
	}

	// Hash the data
	dataRaw, err := HashStructure(req.Data, f.hashFunc())
	if err != nil {
		return err
	}
	data, ok := dataRaw.(map[string]interface{})
	if !ok {
		return fmt.Errorf("data came back as not map")
	}

	// Encode!
	enc := json.NewEncoder(w)
	return enc.Encode(&JSONRequestEntry{
		Type: "request",

		Auth: JSONAuth{
			Policies: auth.Policies,
			Metadata: auth.Metadata,
		},

		Request: JSONRequest{
			Operation: req.Operation,
			Path:      req.Path,
			Data:      data,
		},
	})
}

func (f *FormatJSON) FormatResponse(
	w io.Writer,
	auth *logical.Auth,
	req *logical.Request,
	resp *logical.Response,
	err error) error {
	hashFunc := f.hashFunc()

	// If things are nil, make empty to avoid panics
	if auth == nil {
		auth = new(logical.Auth)
	}
	if resp == nil {
		resp = new(logical.Response)
	}

	var respAuth JSONAuth
	if resp.Auth != nil {
		token, err := hashFunc(resp.Auth.ClientToken)
		if err != nil {
			return err
		}

		respAuth = JSONAuth{
			ClientToken: token,
			Policies:    resp.Auth.Policies,
			Metadata:    resp.Auth.Metadata,
		}
	}

	var respSecret JSONSecret
	if resp.Secret != nil {
		respSecret = JSONSecret{
			LeaseID: resp.Secret.LeaseID,
		}
	}

	// Hash the data
	dataRaw, err := HashStructure(req.Data, f.hashFunc())
	if err != nil {
		return err
	}
	reqData, ok := dataRaw.(map[string]interface{})
	if !ok {
		return fmt.Errorf("data came back as not map")
	}

	dataRaw, err = HashStructure(resp.Data, f.hashFunc())
	if err != nil {
		return err
	}
	respData, ok := dataRaw.(map[string]interface{})
	if !ok {
		return fmt.Errorf("data came back as not map")
	}

	// Encode!
	enc := json.NewEncoder(w)
	return enc.Encode(&JSONResponseEntry{
		Type: "response",

		Auth: JSONAuth{
			Policies: auth.Policies,
			Metadata: auth.Metadata,
		},

		Request: JSONRequest{
			Operation: req.Operation,
			Path:      req.Path,
			Data:      reqData,
		},

		Response: JSONResponse{
			Auth:     respAuth,
			Secret:   respSecret,
			Data:     respData,
			Redirect: resp.Redirect,
		},
	})
}

func (f *FormatJSON) hashFunc() HashCallback {
	return HashSHA1("")
}

// JSONRequest is the structure of a request audit log entry in JSON.
type JSONRequestEntry struct {
	Type    string      `json:"type"`
	Auth    JSONAuth    `json:"auth"`
	Request JSONRequest `json:"request"`
}

// JSONResponseEntry is the structure of a response audit log entry in JSON.
type JSONResponseEntry struct {
	Type     string       `json:"type"`
	Error    string       `json:"error"`
	Auth     JSONAuth     `json:"auth"`
	Request  JSONRequest  `json:"request"`
	Response JSONResponse `json:"response"`
}

type JSONRequest struct {
	Operation logical.Operation      `json:"operation"`
	Path      string                 `json:"path"`
	Data      map[string]interface{} `json:"data"`
}

type JSONResponse struct {
	Auth     JSONAuth               `json:"auth,omitempty"`
	Secret   JSONSecret             `json:"secret,emitempty"`
	Data     map[string]interface{} `json:"data"`
	Redirect string                 `json:"redirect"`
}

type JSONAuth struct {
	ClientToken string            `json:"string,omitempty"`
	Policies    []string          `json:"policies"`
	Metadata    map[string]string `json:"metadata"`
}

type JSONSecret struct {
	LeaseID string `json:"lease_id"`
}
