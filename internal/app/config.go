package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"magic/internal/proto"
)

type Config struct {
	Name        string  `json:"name"`
	Listen      string  `json:"listen"`
	Peer        string  `json:"peer"`
	Secret      string  `json:"secret"`
	Edge        string  `json:"edge"`
	SwapCmdCtrl *bool   `json:"swap_cmd_ctrl"`
	Clipboard   *bool   `json:"clipboard"`
	Panel       string  `json:"panel"`
	FPS         int     `json:"fps"`
	JPEGQuality int     `json:"jpeg_quality"`
	MouseScale  float64 `json:"mouse_scale"`
	// VirtualDisplay false keeps the virtual display a phantom: the pointer
	// still crosses, windows cannot span.
	VirtualDisplay *bool `json:"virtual_display"`
	FileClipMaxMB  int64 `json:"file_clip_max_mb"` // < 0 disables file copy
	// SwitchDelayMS is how long the pointer must push against the shared
	// edge before it crosses; < 0 crosses at once.
	SwitchDelayMS int `json:"switch_delay_ms"`
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, c.Normalize()
}

// Normalize fills defaults and validates.
func (c *Config) Normalize() error {
	t := true
	if c.Name == "" {
		h, _ := os.Hostname()
		c.Name = h
	}
	if c.Listen == "" {
		c.Listen = ":24800"
	}
	if c.Panel == "" {
		c.Panel = "127.0.0.1:24801"
	}
	if c.SwapCmdCtrl == nil {
		c.SwapCmdCtrl = &t
	}
	if c.Clipboard == nil {
		c.Clipboard = &t
	}
	if c.VirtualDisplay == nil {
		c.VirtualDisplay = &t
	}
	if c.FPS <= 0 {
		c.FPS = 15
	}
	if c.JPEGQuality <= 0 || c.JPEGQuality > 100 {
		c.JPEGQuality = 70
	}
	if c.FileClipMaxMB == 0 {
		c.FileClipMaxMB = 2048
	}
	if c.SwitchDelayMS == 0 {
		c.SwitchDelayMS = 120
	}
	if c.MouseScale <= 0 {
		c.MouseScale = 1
	}
	if len(c.Secret) < 12 {
		return errors.New("config: secret must be at least 12 characters")
	}
	if c.Peer == "" {
		return errors.New("config: peer is required")
	}
	if _, err := c.EdgeValue(); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(c.Panel)
	if err != nil {
		return fmt.Errorf("config: panel: %w", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("config: panel must bind a loopback address, got %q", c.Panel)
	}
	return nil
}

func (c *Config) EdgeValue() (proto.Edge, error) {
	switch strings.ToLower(c.Edge) {
	case "left":
		return proto.EdgeLeft, nil
	case "right":
		return proto.EdgeRight, nil
	case "top":
		return proto.EdgeTop, nil
	case "bottom":
		return proto.EdgeBottom, nil
	}
	return 0, fmt.Errorf("config: edge must be left|right|top|bottom, got %q", c.Edge)
}
