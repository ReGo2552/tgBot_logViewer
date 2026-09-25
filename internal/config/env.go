package config

import (
	"bufio"
	"os"
	"strings"
)

// Env — секреты из окружения: в сервисе их кладёт systemd (EnvironmentFile),
// при ручном запуске — LoadEnvFile.
type Env struct {
	Token  string
	Proxy  string
	APIURL string
}

func EnvFromOS() Env {
	return Env{
		Token:  os.Getenv("BOT_TOKEN"),
		Proxy:  os.Getenv("PROXY_URL"),
		APIURL: os.Getenv("API_URL"),
	}
}

// LoadEnvFile читает KEY=VALUE в окружение процесса, не перетирая уже заданное.
func LoadEnvFile(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(strings.TrimPrefix(k, "export "))
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}
