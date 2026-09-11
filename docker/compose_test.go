package docker

import (
	"reflect"
	"testing"
)

func TestGroupComposeProjects(t *testing.T) {
	containers := []Container{
		{ID: "a", State: "running", Labels: map[string]string{
			"com.docker.compose.project":              "webtier",
			"com.docker.compose.service":              "web",
			"com.docker.compose.project.config_files": "docker-compose.yml",
		}},
		{ID: "b", State: "running", Labels: map[string]string{
			"com.docker.compose.project":              "webtier",
			"com.docker.compose.service":              "db",
			"com.docker.compose.project.config_files": "docker-compose.yml",
		}},
		{ID: "c", State: "exited", Labels: map[string]string{
			"com.docker.compose.project": "webtier",
			"com.docker.compose.service": "worker",
		}},
		{ID: "d", State: "running", Labels: map[string]string{ // no compose label
			"some.other": "label",
		}},
		{ID: "e", State: "running", Labels: map[string]string{
			"com.docker.compose.project": "batch",
			"com.docker.compose.service": "cron",
		}},
		{ID: "f", State: "exited", Labels: map[string]string{ // replica of db, fully down service
			"com.docker.compose.project": "webtier",
			"com.docker.compose.service": "cache",
		}},
	}

	got := groupComposeProjects(containers)
	want := []ComposeProject{
		{Name: "batch", Services: []ComposeService{{Name: "cron", Running: true}}, Running: 1, Total: 1, ConfigFiles: ""},
		{Name: "webtier", Services: []ComposeService{
			{Name: "cache", Running: false},
			{Name: "db", Running: true},
			{Name: "web", Running: true},
			{Name: "worker", Running: false},
		}, Running: 2, Total: 4, ConfigFiles: "docker-compose.yml"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("groupComposeProjects =\n%+v\nwant\n%+v", got, want)
	}

	if empty := groupComposeProjects(nil); empty == nil {
		t.Error("empty input must yield an empty slice, not nil")
	}
}

func TestComposeArgs(t *testing.T) {
	p := ComposeProject{Name: "webtier", ConfigFiles: "docker-compose.yml compose.override.yml"}
	if got := composeArgs(p, "up", "-d"); !reflect.DeepEqual(got, []string{
		"compose", "-p", "webtier", "-f", "docker-compose.yml", "-f", "compose.override.yml", "up", "-d",
	}) {
		t.Errorf("composeArgs with config files = %q", got)
	}

	p2 := ComposeProject{Name: "batch"}
	if got := composeArgs(p2, "down"); !reflect.DeepEqual(got, []string{"compose", "-p", "batch", "down"}) {
		t.Errorf("composeArgs without config files = %q", got)
	}
}

func TestComposeServiceArgs(t *testing.T) {
	c := &Client{}
	p := ComposeProject{Name: "webtier", ConfigFiles: "docker-compose.yml"}

	tests := []struct {
		name string
		got  []string
		want []string
	}{
		{"logs", c.ComposeServiceLogsArgs(p, "cache"), []string{"compose", "-p", "webtier", "-f", "docker-compose.yml", "logs", "-f", "--tail=100", "cache"}},
		{"exec", c.ComposeServiceExecArgs(p, "api"), []string{"compose", "-p", "webtier", "-f", "docker-compose.yml", "exec", "api", "sh"}},
	}
	for _, tt := range tests {
		if !reflect.DeepEqual(tt.got, tt.want) {
			t.Errorf("%s args = %q, want %q", tt.name, tt.got, tt.want)
		}
	}

	p2 := ComposeProject{Name: "batch"}
	if got := c.ComposeServiceExecArgs(p2, "cron"); !reflect.DeepEqual(got, []string{"compose", "-p", "batch", "exec", "cron", "sh"}) {
		t.Errorf("exec args without config files = %q", got)
	}
}
