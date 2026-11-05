package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Child battle server executables are fixed: they must sit next to the proxy
// binary itself (not the working directory) under these exact names.
const (
	exe1Name = "BattleServerOrig.exe"
	exe2Name = "BattleServerReimpl.exe"
)

type Config struct {
	Exe1Path           string
	Exe2Path           string
	Exe1ExtraArgs      string
	Exe2ExtraArgs      string
	ProxyHost          string
	ProxyPort          int
	ProxyWSPort        int
	ProxyOOBPort       int
	RelayBroadcastPort int
	Target1Host        string
	Target1Port        int
	Target1WSPort      int
	Target1OOBPort     int
	Target2Host        string
	Target2Port        int
	Target2WSPort      int
	Target2OOBPort     int
	MainLogPath        string
	SecondaryLogPath   string
	LogFilePath        string
	MitmTLS            bool
	MitmCert           string
	MitmKey            string
	MitmKeyLog         string
}

func getEnv(key, fallback string) string {
	if val, ok := os.LookupEnv(key); ok && val != "" {
		return val
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if val, ok := os.LookupEnv(key); ok && val != "" {
		if b, err := strconv.ParseBool(val); err == nil {
			return b
		}
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if val, ok := os.LookupEnv(key); ok && val != "" {
		if intVal, err := strconv.Atoi(val); err == nil {
			return intVal
		}
	}
	return fallback
}

// exeDir returns the directory containing the running proxy executable,
// falling back to the working directory when it cannot be determined.
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

func loadConfig() Config {
	exeDir := exeDir()
	return Config{
		Exe1Path:           filepath.Join(exeDir, exe1Name),
		Exe2Path:           filepath.Join(exeDir, exe2Name),
		Exe1ExtraArgs:      getEnv("EXE1_ARGS", "-relaybroadcastPort 0 -bsPort 14001 -outofbandPort 14002 -websocketPort 14003"),
		Exe2ExtraArgs:      getEnv("EXE2_ARGS", "-relaybroadcastPort 0 -bsPort 15001 -outofbandPort 15002 -websocketPort 15003"),
		ProxyHost:          getEnv("PROXY_HOST", "0.0.0.0"),
		ProxyPort:          getEnvInt("PROXY_PORT", 13001),
		ProxyWSPort:        getEnvInt("PROXY_WS_PORT", 13003),
		ProxyOOBPort:       getEnvInt("PROXY_OOB_PORT", 13002),
		RelayBroadcastPort: getEnvInt("RELAY_BROADCAST_PORT", -1),
		Target1Host:        getEnv("TARGET1_HOST", "127.0.0.1"),
		Target1Port:        getEnvInt("TARGET1_PORT", 14001),
		Target1WSPort:      getEnvInt("TARGET1_WS_PORT", 14003),
		Target1OOBPort:     getEnvInt("TARGET1_OOB_PORT", 14002),
		Target2Host:        getEnv("TARGET2_HOST", "127.0.0.1"),
		Target2Port:        getEnvInt("TARGET2_PORT", 15001),
		Target2WSPort:      getEnvInt("TARGET2_WS_PORT", 15003),
		Target2OOBPort:     getEnvInt("TARGET2_OOB_PORT", 15002),
		MainLogPath:        getEnv("MAIN_LOG_PATH", "main_log.csv"),
		SecondaryLogPath:   getEnv("SECONDARY_LOG_PATH", "secondary_log.csv"),
		LogFilePath:        getEnv("LOG_FILE_PATH", "proxy.log"),
		MitmTLS:            getEnvBool("MITM_TLS", true),
		MitmCert:           getEnv("MITM_CERT", filepath.Join(exeDir, "certs", "selfsigned_cert.pem")),
		MitmKey:            getEnv("MITM_KEY", filepath.Join(exeDir, "certs", "selfsigned_key.pem")),
		MitmKeyLog:         getEnv("MITM_KEYLOG", ""),
	}
}

// splitServerPorts removes the listen-port arguments (-bsPort,
// -webSocketPort, -outOfBandPort, -relayBroadcastPort) and the certificate
// arguments (-sslCert, -sslKey) from the command line: the proxy owns those
// ports and the MITM certificate, and they are never forwarded to the child
// processes, whose listen ports are fixed. It returns the filtered args, the
// ports found and the certificate/key paths.
func splitServerPorts(args []string) (filtered []string, bsPort, wsPort, oobPort, relayPort int, certPath, keyPath string) {
	bsPort, wsPort, oobPort, relayPort = -1, -1, -1, -1
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			filtered = append(filtered, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		val := ""
		hasVal := false
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			val, hasVal, name = name[eq+1:], true, name[:eq]
		}
		var target int
		switch strings.ToLower(name) {
		case "bsport":
			target = 1
		case "websocketport":
			target = 2
		case "outofbandport":
			target = 3
		case "relaybroadcastport":
			target = 4
		case "sslcert":
			target = 5
		case "sslkey":
			target = 6
		}
		if target == 0 {
			filtered = append(filtered, a)
			continue
		}
		if !hasVal {
			if i+1 >= len(args) {
				continue
			}
			val = args[i+1]
			i++
		}
		switch target {
		case 1:
			if n, err := strconv.Atoi(val); err == nil {
				bsPort = n
			}
		case 2:
			if n, err := strconv.Atoi(val); err == nil {
				wsPort = n
			}
		case 3:
			if n, err := strconv.Atoi(val); err == nil {
				oobPort = n
			}
		case 4:
			if n, err := strconv.Atoi(val); err == nil {
				relayPort = n
			}
		case 5:
			certPath = val
		default:
			keyPath = val
		}
	}
	return filtered, bsPort, wsPort, oobPort, relayPort, certPath, keyPath
}
