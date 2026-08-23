package docker

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

type Container struct {
	ID      string   `json:"Id"`
	Names   []string `json:"Names"`
	Image   string   `json:"Image"`
	ImageID string   `json:"ImageID"`
	State   string   `json:"State"`
	Status  string   `json:"Status"`
	Ports   []Port   `json:"Ports"`
}

type Port struct {
	IP          string `json:"IP"`
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort"`
	Type        string `json:"Type"`
}

type versionInfo struct {
	APIVersion string `json:"ApiVersion"`
}

type Client struct {
	baseURL   string
	apiPrefix string
	http      http.Client
}

func NewClient() (*Client, error) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}

	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("invalid DOCKER_HOST: %w", err)
	}

	c := &Client{baseURL: host}

	switch u.Scheme {
	case "unix":
		c.http = http.Client{
			Transport: &http.Transport{
				Dial: func(proto, addr string) (net.Conn, error) {
					return net.Dial("unix", u.Path)
				},
			},
		}
		c.baseURL = "http://docker"
	case "tcp":
		c.http = http.Client{}
		c.baseURL = fmt.Sprintf("http://%s", u.Host)
	default:
		return nil, fmt.Errorf("unsupported Docker host scheme: %s", u.Scheme)
	}

	// Detect API version
	ver, err := c.detectAPIVersion()
	if err != nil {
		return nil, fmt.Errorf("detecting Docker API version: %w", err)
	}
	c.apiPrefix = fmt.Sprintf("/v%s", ver)

	return c, nil
}

func (c *Client) detectAPIVersion() (string, error) {
	resp, err := c.http.Get(c.baseURL + "/version")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var v versionInfo
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	if v.APIVersion == "" {
		return "1.45", nil
	}
	return v.APIVersion, nil
}

func (c *Client) do(method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, c.baseURL+c.apiPrefix+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker request failed: %w", err)
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("Docker API error: %s", resp.Status)
	}
	return resp, nil
}

func (c *Client) ListContainers(all bool) ([]Container, error) {
	allFlag := "0"
	if all {
		allFlag = "1"
	}
	resp, err := c.do("GET", fmt.Sprintf("/containers/json?all=%s", allFlag), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var containers []Container
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, fmt.Errorf("decoding containers: %w", err)
	}
	return containers, nil
}

func (c *Client) StartContainer(id string) error {
	_, err := c.do("POST", fmt.Sprintf("/containers/%s/start", id), nil)
	return err
}

func (c *Client) StopContainer(id string) error {
	_, err := c.do("POST", fmt.Sprintf("/containers/%s/stop?t=10", id), nil)
	return err
}

func (c *Client) RestartContainer(id string) error {
	_, err := c.do("POST", fmt.Sprintf("/containers/%s/restart?t=10", id), nil)
	return err
}

func (c *Client) ContainerLogs(id, tail string, follow bool) (io.ReadCloser, error) {
	followFlag := "0"
	if follow {
		followFlag = "1"
	}
	resp, err := c.do("GET", fmt.Sprintf("/containers/%s/logs?stdout=1&stderr=1&timestamps=1&tail=%s&follow=%s", id, url.QueryEscape(tail), followFlag), nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (c *Client) Ping() error {
	resp, err := c.http.Get(c.baseURL + "/_ping")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Client) Close() error {
	return nil
}

// StripDockerStreamHeaders removes the 8-byte Docker stream header from log frames.
func StripDockerStreamHeaders(data []byte) string {
	var result strings.Builder
	for len(data) > 0 {
		if len(data) < 8 {
			result.Write(data)
			break
		}
		payloadLen := int(data[4])<<24 | int(data[5])<<16 | int(data[6])<<8 | int(data[7])
		if payloadLen > len(data)-8 {
			payloadLen = len(data) - 8
		}
		if payloadLen == 0 {
			data = data[8:]
			continue
		}
		result.Write(data[8 : 8+payloadLen])
		data = data[8+payloadLen:]
	}
	return result.String()
}