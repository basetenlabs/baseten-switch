package main

import (
	"io"
	"net/http"
)

type doctorAdminStatus struct {
	Clients []doctorAdminClient `json:"clients"`
}

type doctorAdminClient struct {
	Name           string `json:"name"`
	Enabled        bool   `json:"enabled"`
	BindAddr       string `json:"bind_addr"`
	Route          string `json:"effective_route"`
	FallbackRoute  string `json:"fallback_route"`
	CurrentlyBound bool   `json:"currently_bound"`
}

func doctorAdminGetOK(c *http.Client, admin, path string) (string, bool) {
	resp, err := c.Get("http://" + admin + path)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	return string(b), true
}
