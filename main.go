package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	_ "github.com/mritd/logrus"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var conf TPClashConf

var build string
var commit string
var version string
var clash string

var rootCmd = &cobra.Command{
	Use:   "tpclash",
	Short: "Transparent proxy tool for Clash",
	Run: func(_ *cobra.Command, _ []string) {
		if conf.PrintVersion {
			fmt.Printf("%s\nVersion: %s\nBuild: %s\nClash Core: %s\nCommit: %s\n", logo, version, build, clash, commit)
			return
		}

		if conf.Debug {
			logrus.SetLevel(logrus.DebugLevel)
		}

		logrus.Info("[main] starting tpclash...")

		// Initialize signal control Context
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		defer cancel()

		// Configure Sysctl
		Sysctl()

		// Extract Clash executable and built-in configuration files
		ExtractFiles(&conf)

		// Watch config file
		updateCh := WatchConfig(ctx, &conf)

		// Wait for the first config to return
		clashConfStr := <-updateCh

		// Check clash config
		if _, err := CheckConfig(clashConfStr); err != nil {
			logrus.Fatal(err)
		}

		// Copy remote or local clash config file to internal path
		clashConfPath := filepath.Join(conf.ClashHome, InternalConfigName)
		ClashUIPath := filepath.Join(conf.ClashHome, conf.ClashUI)
		clashBinPath := filepath.Join(conf.ClashHome, InternalClashBinName)
		if err := os.WriteFile(clashConfPath, []byte(clashConfStr), 0644); err != nil {
			logrus.Fatalf("[main] failed to copy clash config: %v", err)
		}

		// Create child process
		cmd := exec.Command(clashBinPath, "-f", clashConfPath, "-d", conf.ClashHome, "-ext-ui", ClashUIPath)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{
			AmbientCaps: []uintptr{CAP_NET_BIND_SERVICE, CAP_NET_ADMIN, CAP_NET_RAW},
		}
		logrus.Infof("[main] running cmds: %v", cmd.Args)

		if err := cmd.Start(); err != nil {
			logrus.Error(err)
			cancel()
		}
		if cmd.Process == nil {
			logrus.Errorf("failed to start clash process: %v", cmd.Args)
			cancel()
		}

		proxyMode, err := NewProxyMode(&conf)
		if err != nil {
			logrus.Fatalf("[main] failed to create proxy mode: %v", err)
		}

		if err := proxyMode.EnableProxy(); err != nil {
			logrus.Fatalf("[main] failed to enable proxy: %v", err)
		}

		// Watch clash config changes, and automatically reload the config
		go func() {
			for ccStr := range updateCh {
				logrus.Info("[main] clash config changed, reloading...")

				ccStr = AutoFix(ccStr)
				cc, err := CheckConfig(ccStr)
				if err != nil {
					logrus.Errorf("[main] an error was detected in the clash config, skipping automatic reload:\n %v", err)
					continue
				}

				if err := os.WriteFile(clashConfPath, []byte(ccStr), 0644); err != nil {
					logrus.Errorf("[main] failed to copy clash config: %v", err)
					continue
				}

				apiAddr := cc.ExternalController
				if apiAddr == "" {
					apiAddr = "127.0.0.1:9090"
				}
				secret := cc.Secret

				req, err := http.NewRequest("PUT", "http://"+apiAddr+"/configs", bytes.NewReader([]byte(fmt.Sprintf(`{"path": "%s"}`, clashConfPath))))
				if err != nil {
					logrus.Errorf("[main] failed to create reload req: %v", err)
					continue
				}
				req.Header.Set("Authorization", "Bearer "+secret)
				cli := &http.Client{}

				resp, err := cli.Do(req)
				if err != nil {
					logrus.Errorf("[main] failed to reload config: %v", err)
					continue
				}
				defer func() { _ = resp.Body.Close() }()

				if !(resp.StatusCode >= 200 && resp.StatusCode <= 299) {
					var msg bytes.Buffer
					_, _ = io.Copy(&msg, resp.Body)
					logrus.Errorf("[main] failed to reload config: status %d: %s", resp.StatusCode, msg.String())
				}

				logrus.Info("[main] clash config reload success...")
			}
		}()

		logrus.Info("[main] 🍄 提莫队长正在待命...")

		<-ctx.Done()
		logrus.Info("[main] 🛑 TPClash 正在停止...")

		if err := proxyMode.DisableProxy(); err != nil {
			logrus.Error(err)
		}

		if cmd.Process != nil {
			if err := cmd.Process.Kill(); err != nil {
				logrus.Error(err)
			}
		}

		logrus.Info("[main] 🛑 TPClash 已关闭!")
	},
}

func init() {
	cobra.EnableCommandSorting = false

	rootCmd.PersistentFlags().BoolVar(&conf.Debug, "debug", false, "enable debug log")
	rootCmd.PersistentFlags().StringVarP(&conf.ClashHome, "home", "d", "/data/clash", "clash home dir")
	rootCmd.PersistentFlags().StringVarP(&conf.ClashConfig, "config", "c", "/etc/clash.yaml", "clash config local path or remote url")
	rootCmd.PersistentFlags().StringVarP(&conf.ClashUI, "ui", "u", "yacd", "clash dashboard(official|yacd)")
	rootCmd.PersistentFlags().DurationVarP(&conf.CheckInterval, "check-interval", "i", 120*time.Second, "remote config check interval")
	rootCmd.PersistentFlags().StringSliceVar(&conf.HttpHeader, "http-header", []string{}, "http header when requesting a remote config(key=value)")
	rootCmd.PersistentFlags().BoolVar(&conf.DisableExtract, "disable-extract", false, "disable extract files")
	rootCmd.PersistentFlags().BoolVarP(&conf.PrintVersion, "version", "v", false, "version for tpclash")
}

func main() {
	cobra.CheckErr(rootCmd.Execute())
}
