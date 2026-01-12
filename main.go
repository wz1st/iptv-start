package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"start/until"
	"time"

	"golang.org/x/net/websocket"
)

var VERSION = "2.0.0"

var (
	BIN_PATH    = "/app"
	LICENSE_CMD *exec.Cmd
	IPTV_CMD    *exec.Cmd
)

func main() {
	log.Println("升级服务版本号:", VERSION)
	checkIptv()
	if !until.IsPrivileged() {
		log.Println("请使用privileged(特权模式、高权限执行容器)运行")
		return
	}
	os.MkdirAll("/tmp/check_privileged", 0755)
	if until.CheckRam() {
		log.Println("可用内存不足256MB，无法运行")
		return
	}
	os.MkdirAll("/tmp/check_start_ram", 0755)

	if !until.Exists("/config") {
		log.Println("请映射config文件夹到容器/config中")
		return
	}

	err := os.Chmod("/config", 0777)
	if err != nil {
		log.Println("/config文件夹权限设置失败,请手动设置")
		return
	}
	err = until.FixPerm("/config")
	if err != nil {
		log.Println("/config文件夹权限设置失败,请手动设置")
		return
	}
	if startLicense() {
		if startIPTV() {
			// 监听升级信号
			// http.HandleFunc("/update", updateHandler)
			http.HandleFunc("/licRestart", licRestart)
			port := 82
			log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", port), nil))
		}
	} else {
		log.Fatal(errors.New("启动失败"))
	}

}

func licRestart(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/licRestart" {
		http.NotFound(w, r)
		return
	}
	if LICENSE_CMD != nil {
		_ = LICENSE_CMD.Process.Kill()
		_ = LICENSE_CMD.Wait()
		LICENSE_CMD = nil
	}

	if startLicense() {
		log.Println("引擎重启成功")
		fmt.Fprintln(w, "OK")
		return
	}
	fmt.Fprintln(w, "FAIL")
}

func checkIptv() {
	if !until.Exists("/app/license") || !until.Exists("/app/iptv") {
		log.Fatal("镜像不完整，请重新拉取镜像")
	}
}

// 启动引擎
func startLicense() bool {

	if !until.Exists("/config/iptv.db") || !until.Exists("/config/config.yml") || !until.Exists("/config/install.lock") {
		log.Println("系统未安装，跳过启动引擎")
		return true
	}

	if LICENSE_CMD != nil {
		return true
	}
	log.Println("启动引擎...")

	cmd := exec.Command(BIN_PATH + "/license")

	// 创建日志文件并覆盖原有内容
	logFile, err := os.OpenFile("/config/license.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		log.Printf("无法打开/config/license.log日志文件,请检查目录权限: %v", err)
		return false
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		log.Printf("启动 license 失败: %v", err)
		return false
	}
	LICENSE_CMD = cmd
	log.Printf("引擎已启动，PID=%d", cmd.Process.Pid)
	return waitLicense()
}

// 等待引擎启动完成 (通过 WebSocket ping)
func waitLicense() bool {
	url := "ws://127.0.0.1:81/ws"
	timeout := 60 * time.Second
	start := time.Now()
	log.Println("等待引擎启动完成...")

	for {
		ws, err := websocket.Dial(url, "", "http://127.0.0.1:81/")
		if err == nil {
			// 发送 ping 消息
			msg := []byte("ping")
			if _, err := ws.Write(msg); err == nil {
				ws.Close()
				log.Println("引擎启动完成")
				return true
			}
			ws.Close()
		}

		if time.Since(start) > timeout {
			log.Println("等待引擎超时")
			return false
		}
		time.Sleep(1 * time.Second)
	}
}

// 启动 IPTV 并输出日志到容器 stdout
func startIPTV() bool {
	if IPTV_CMD != nil {
		return true
	}
	log.Println("启动 IPTV...")

	cmd := exec.Command(BIN_PATH + "/iptv")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Fatalf("启动 IPTV 失败: %v", err)
	}
	IPTV_CMD = cmd
	log.Printf("IPTV 已启动")
	return true
}
