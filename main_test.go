package main

import (
	"net/url"
	"testing"
)

func TestBuildURL(t *testing.T) {
	tests := []struct {
		name       string
		cfg        config
		wantHost   string
		wantPath   string
		wantUser   string
		wantPass   string
		wantNoUser bool
	}{
		{
			name:       "no credentials, default port",
			cfg:        config{host: "10.0.0.5", port: 554, name: "live/ch0"},
			wantHost:   "10.0.0.5:554",
			wantPath:   "/live/ch0",
			wantNoUser: true,
		},
		{
			name:     "credentials and non-default port",
			cfg:      config{host: "camera.local", port: 8554, user: "admin", pass: "secret", name: "Stream1"},
			wantHost: "camera.local:8554",
			wantPath: "/Stream1",
			wantUser: "admin",
			wantPass: "secret",
		},
		{
			name:     "credentials with url-significant characters",
			cfg:      config{host: "192.168.1.50", port: 554, user: "a@b", pass: "p@ss:w/rd?%#", name: "h264Preview_01_main"},
			wantHost: "192.168.1.50:554",
			wantPath: "/h264Preview_01_main",
			wantUser: "a@b",
			wantPass: "p@ss:w/rd?%#",
		},
		{
			name:       "leading slash in name is not doubled",
			cfg:        config{host: "cam", port: 554, name: "/live"},
			wantHost:   "cam:554",
			wantPath:   "/live",
			wantNoUser: true,
		},
		{
			name:       "spaces in name are escaped",
			cfg:        config{host: "cam", port: 554, name: "front door"},
			wantHost:   "cam:554",
			wantPath:   "/front door",
			wantNoUser: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := buildURL(tt.cfg)
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("buildURL(%+v) returned unparseable URL %q: %v", tt.cfg, raw, err)
			}
			if u.Scheme != "rtsp" {
				t.Errorf("scheme = %q, want %q", u.Scheme, "rtsp")
			}
			if u.Host != tt.wantHost {
				t.Errorf("host = %q, want %q", u.Host, tt.wantHost)
			}
			if u.Path != tt.wantPath {
				t.Errorf("path = %q, want %q", u.Path, tt.wantPath)
			}
			if tt.wantNoUser {
				if u.User != nil {
					t.Errorf("userinfo = %v, want none", u.User)
				}
				return
			}
			if u.User == nil {
				t.Fatalf("userinfo missing from %q", raw)
			}
			if got := u.User.Username(); got != tt.wantUser {
				t.Errorf("username = %q, want %q", got, tt.wantUser)
			}
			if got, _ := u.User.Password(); got != tt.wantPass {
				t.Errorf("password = %q, want %q", got, tt.wantPass)
			}
		})
	}
}

func TestNormalizeArgs(t *testing.T) {
	tests := []struct {
		in   []string
		want []string
	}{
		{
			in:   []string{"192.168.1.50", "-u", "admin", "-n", "live"},
			want: []string{"-u", "admin", "-n", "live", "192.168.1.50"},
		},
		{
			in:   []string{"-u", "admin", "host"},
			want: []string{"-u", "admin", "host"},
		},
		{
			in:   []string{"-p=8554", "host", "-n", "live"},
			want: []string{"-p=8554", "-n", "live", "host"},
		},
		{
			in:   []string{"-pw", "-secret", "host"},
			want: []string{"-pw", "-secret", "host"},
		},
		{
			in:   []string{"--", "host"},
			want: []string{"--", "host"},
		},
		{
			in:   []string{"-u", "admin", "--", "host"},
			want: []string{"-u", "admin", "--", "host"},
		},
		{
			in:   []string{"host", "--", "-weird"},
			want: []string{"--", "host", "-weird"},
		},
	}
	for _, tt := range tests {
		got := normalizeArgs(tt.in)
		if len(got) != len(tt.want) {
			t.Fatalf("normalizeArgs(%q) = %q; want %q", tt.in, got, tt.want)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Fatalf("normalizeArgs(%q) = %q; want %q", tt.in, got, tt.want)
			}
		}
	}
}

func TestCaptureSize(t *testing.T) {
	tests := []struct {
		w, h, maxDim int
		wantW, wantH int
		wantFilter   string
	}{
		{640, 360, 4096, 640, 360, ""},
		{3840, 2160, 4096, 3840, 2160, ""},
		{4096, 2160, 4096, 4096, 2160, ""},
		{8192, 4320, 4096, 4096, 2160, "scale=4096:2160"},
		{5000, 3000, 4096, 4096, 2456, "scale=4096:2456"},
	}
	for _, tt := range tests {
		gotW, gotH, gotFilter := captureSize(tt.w, tt.h, tt.maxDim)
		if gotW != tt.wantW || gotH != tt.wantH || gotFilter != tt.wantFilter {
			t.Errorf("captureSize(%d, %d, %d) = %d, %d, %q; want %d, %d, %q",
				tt.w, tt.h, tt.maxDim, gotW, gotH, gotFilter, tt.wantW, tt.wantH, tt.wantFilter)
		}
	}
}

func TestFitWindow(t *testing.T) {
	tests := []struct {
		w, h, maxW, maxH int
		wantW, wantH     int
	}{
		{640, 360, 1280, 720, 640, 360},
		{1280, 720, 1280, 720, 1280, 720},
		{1920, 1080, 1280, 720, 1280, 720},
		{3840, 2160, 1280, 720, 1280, 720},
		{720, 1280, 1280, 720, 405, 720},
	}
	for _, tt := range tests {
		gotW, gotH := fitWindow(tt.w, tt.h, tt.maxW, tt.maxH)
		if gotW != tt.wantW || gotH != tt.wantH {
			t.Errorf("fitWindow(%d, %d, %d, %d) = %d, %d; want %d, %d",
				tt.w, tt.h, tt.maxW, tt.maxH, gotW, gotH, tt.wantW, tt.wantH)
		}
	}
}
