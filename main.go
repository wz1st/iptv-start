package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"start/until"
	"strings"
	"time"

	"golang.org/x/net/websocket"
)

var VERSION = "1.0.0"

var (
	BIN_PATH    = "/config/bin"
	WATCH_DIR   = "/config/updata"
	LICENSE_CMD *exec.Cmd
	IPTV_CMD    *exec.Cmd
)

func main() {
	log.Println("升级服务版本号:", VERSION)
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

	// 启动服务器
	if updata(true) {
		// 监听升级信号
		http.HandleFunc("/update", updateHandler)
		http.HandleFunc("/licRestart", licRestart)
		port := 82
		log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", port), nil))
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

func checkLicense() error {
	if !Exists("/app/Version_lic") || !Exists("/app/license") {
		log.Fatal("镜像不完整，请重新拉取镜像")
	}
	if !Exists(BIN_PATH+"/Version_lic") || !Exists(BIN_PATH+"/license") {
		os.Remove(BIN_PATH + "/license")
		os.Remove(BIN_PATH + "/Version_lic")
		if err := os.MkdirAll(BIN_PATH, 0755); err != nil {
			log.Println("/config/bin/ 创建目录失败:" + err.Error())
			return errors.New("/config/bin/ 创建目录失败,请检查目录权限并删除/config/bin/和/config/updata目录")
		}
		if err := copyAndChmod("/app/license", BIN_PATH+"/license"); err != nil {
			log.Println("复制文件license失败:" + err.Error())
			return errors.New("复制文件license失败,请检查目录权限并删除/config/bin/和/config/updata目录")
		}
		if err := copyAndChmod("/app/Version_lic", BIN_PATH+"/Version_lic"); err != nil {
			log.Println("复制文件Version_lic失败:" + err.Error())
			return errors.New("复制文件Version_lic失败,请检查目录权限并删除/config/bin/和/config/updata目录")
		}
		return nil
	}
	return nil
}

func checkIptv() error {
	if !Exists("/app/Version") || !Exists("/app/iptv") {
		log.Fatal("镜像不完整，请重新拉取镜像")
	}
	if !Exists(BIN_PATH+"/Version") || !Exists(BIN_PATH+"/iptv") {
		os.Remove(BIN_PATH + "/iptv")
		os.Remove(BIN_PATH + "/Version")
		if err := os.MkdirAll(BIN_PATH, 0755); err != nil {
			log.Println("/config/bin/ 创建目录失败:" + err.Error())
			return errors.New("/config/bin/ 创建目录失败,请检查目录权限并删除/config/bin/和/config/updata目录")
		}
		if err := copyAndChmod("/app/iptv", BIN_PATH+"/iptv"); err != nil {
			log.Println("复制文件iptv失败:" + err.Error())
			return errors.New("复制文件iptv失败,请检查目录权限并删除/config/bin/和/config/updata目录")
		}
		if err := copyAndChmod("/app/Version", BIN_PATH+"/Version"); err != nil {
			log.Println("复制文件Version失败:" + err.Error())
			return errors.New("复制文件Version失败,请检查目录权限并删除/config/bin/和/config/updata目录")
		}
		return nil
	}
	return nil
}

// 启动引擎
func startLicense() bool {

	if !until.Exists("/config/iptv.db") || !until.Exists("/config/config.yml") || !until.Exists("/config/install.lock") {
		log.Println("系统未安装，跳过启动引擎")
		return false
	}

	if LICENSE_CMD != nil {
		return true
	}
	log.Println("启动引擎...")

	if checkLicense() != nil {
		log.Fatal("引擎初始化失败")
		return false
	}

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
	if checkIptv() != nil {
		log.Fatal("管理系统初始化失败")
		return false
	}

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

// 更新处理函数
func updateHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/update" {
		http.NotFound(w, r)
		return
	}
	if _, err := os.Stat(WATCH_DIR); os.IsNotExist(err) {
		log.Println("更新目录不存在，跳过")
		fmt.Fprintln(w, "OK")
		return
	}
	if updata(false) {
		fmt.Fprintln(w, "OK")
	} else {
		fmt.Fprintln(w, "FAIL")
	}

}
func updata(boot bool) bool {

	if boot {
		log.Println("系统启动中...")
		licUp := false
		webUp := false

		if !Exists(BIN_PATH+"/Version_lic") || !Exists(BIN_PATH+"/license") {
			os.Remove(BIN_PATH + "/license")
			os.Remove(BIN_PATH + "/Version_lic")
			log.Println("引擎文件不完整，初始化引擎")
			startLicense()
		}

		if !Exists(BIN_PATH+"/Version") || !Exists(BIN_PATH+"/iptv") {
			os.Remove(BIN_PATH + "/iptv")
			os.Remove(BIN_PATH + "/Version")
			log.Println("管理系统文件不完整，初始化管理系统")
			startIPTV()
			return true
		}

		// 判断版本
		newLicVersion := ReadFile("/app/Version_lic")
		curLicVersion := ReadFile(BIN_PATH + "/Version_lic")
		if curLicVersion != "" {
			if curLicVersion == "local" {
				log.Println("引擎为本地版本，跳过版本检查")
				startLicense()
			}
			switch newLicVersion {
			case "":
				log.Fatal("镜像引擎版本文件不存在") //				return false
				return false
			case curLicVersion:
				startLicense()
			default:
				check, err := isNewer(newLicVersion, curLicVersion, 3, true)
				if err != nil {
					if err.Error() == "low" {
						startLicense()
					} else {
						licUp = true
					}
				}
				if check {
					licUp = true
				}
			}
		} else {
			licUp = true
			if !until.Exists("/config/iptv.db") || !until.Exists("/config/config.yml") || !until.Exists("/config/install.lock") {
				licUp = false
			}

		}

		if licUp {
			if LICENSE_CMD != nil {
				_ = LICENSE_CMD.Process.Kill()
				_ = LICENSE_CMD.Wait()
				LICENSE_CMD = nil
			}
			os.Remove(BIN_PATH + "/license")
			os.Remove(BIN_PATH + "/Version_lic")
			startLicense()
		}

		newWebVersion := ReadFile("/app/Version")
		curWebVersion := ReadFile(BIN_PATH + "/Version")
		if curWebVersion != "" {
			if curWebVersion == "local" {
				log.Println("管理系统为本地版本，跳过版本检查")
				return startIPTV()
			}
			switch newWebVersion {
			case "":
				log.Fatal("镜像系统文件不存在")
				return false
			case curWebVersion:
				return startIPTV()
			default:
				check, err := isNewer(newWebVersion, curWebVersion, 4, true)
				if err != nil {
					if err.Error() == "low" {
						return startIPTV()
					}
					webUp = true
				}
				if check {
					webUp = true
				}
			}
		} else {
			webUp = true
		}

		if webUp {
			if IPTV_CMD != nil {
				_ = IPTV_CMD.Process.Kill()
				_ = IPTV_CMD.Wait()
				IPTV_CMD = nil
			}
			os.Remove(BIN_PATH + "/iptv")
			os.Remove(BIN_PATH + "/Version")
			return startIPTV()
		}
	}
	if _, err := os.Stat(WATCH_DIR); os.IsNotExist(err) {
		log.Println("更新目录不存在，跳过")
		startLicense()
		startIPTV()
		return false
	}
	if !Exists(WATCH_DIR+"/Version_lic") || !Exists(WATCH_DIR+"/license") {
		os.Remove(WATCH_DIR + "/license")
		os.Remove(WATCH_DIR + "/Version_lic")
		log.Println("引擎更新文件不完整，跳过")
		startLicense()
	}

	if !Exists(WATCH_DIR+"/Version") || !Exists(WATCH_DIR+"/iptv") {
		os.Remove(WATCH_DIR + "/iptv")
		os.Remove(WATCH_DIR + "/Version")
		log.Println("管理系统更新文件不完整，跳过")
		startIPTV()
	}

	log.Println("开始更新")

	licUp := false
	webUp := false

	// 判断版本
	newLicVersion := ReadFile(WATCH_DIR + "/Version_lic")
	curLicVersion := ReadFile(BIN_PATH + "/Version_lic")

	switch newLicVersion {
	case "":
		log.Println("引擎版本文件不存在，跳过")
		startLicense()
	case curLicVersion:
		log.Println("引擎为最新版本，跳过更新")
		startLicense()
	default:
		check, err := isNewer(newLicVersion, curLicVersion, 3, false)
		if err != nil {
			log.Println(err.Error())
			startLicense()
		}
		if check {
			licUp = true
		}
	}

	if licUp {
		// 更新 license
		license := WATCH_DIR + "/license"
		verUpdate := WATCH_DIR + "/Version_lic"
		_, err1 := os.Stat(license)
		_, err2 := os.Stat(verUpdate)
		if err1 == nil && err2 == nil {
			if LICENSE_CMD != nil {
				_ = LICENSE_CMD.Process.Kill()
				_ = LICENSE_CMD.Wait()
				LICENSE_CMD = nil
			}
			os.Remove(BIN_PATH + "/license")
			os.Remove(BIN_PATH + "/Version_lic")

			log.Println("更新引擎...")

			if err := os.MkdirAll(BIN_PATH, 0755); err != nil {
				log.Fatalln("复制启动文件失败，请检查目录权限")
			}
			lic := BIN_PATH + "/license"
			if err := copyAndChmod(license, lic); err != nil {
				os.Remove(WATCH_DIR + "/license")
				os.Remove(WATCH_DIR + "/Version_lic")
				log.Printf("复制 引擎文件 失败: %v", err)
			} else {
				ver := BIN_PATH + "/Version_lic"
				if err := copyAndChmod(verUpdate, ver); err != nil {
					os.Remove(WATCH_DIR + "/license")
					os.Remove(WATCH_DIR + "/Version_lic")
					log.Printf("复制 引擎版本文件 失败: %v", err)
				}
			}
			startLicense()

		} else {
			os.Remove(WATCH_DIR + "/license")
			os.Remove(WATCH_DIR + "/Version_lic")
			log.Println("引擎文件不存在，跳过更新")
			startLicense()
		}
	}

	newWebVersion := ReadFile(WATCH_DIR + "/Version")
	curWebVersion := ReadFile(BIN_PATH + "/Version")
	switch newWebVersion {
	case "":
		log.Println("管理系统版本文件不存在，跳过")
		startIPTV()

	case curWebVersion:
		log.Println("管理系统为最新版本，跳过更新")
		startIPTV()
	default:
		check, err := isNewer(newWebVersion, curWebVersion, 4, false)
		if err != nil {
			log.Println(err.Error())
			startIPTV()
		}
		if check {
			webUp = true
		}
	}

	if webUp {
		// 更新 IPTV
		iptv := WATCH_DIR + "/iptv"
		verUpdate := WATCH_DIR + "/Version"
		_, err1 := os.Stat(iptv)
		_, err2 := os.Stat(verUpdate)
		if err1 == nil && err2 == nil {
			if IPTV_CMD != nil {
				_ = IPTV_CMD.Process.Kill()
				_ = IPTV_CMD.Wait()
				IPTV_CMD = nil
			}
			os.Remove(BIN_PATH + "/iptv")
			os.Remove(BIN_PATH + "/Version")

			log.Println("更新管理系统...")

			if err := os.MkdirAll(BIN_PATH, 0755); err != nil {
				log.Fatalln("复制启动文件失败，请检查目录权限")
			}

			dst := BIN_PATH + "/iptv"
			if err := copyAndChmod(iptv, dst); err != nil {
				os.Remove(WATCH_DIR + "/iptv")
				os.Remove(WATCH_DIR + "/Version")
				log.Printf("复制 管理系统文件 失败: %v ", err)

			} else {
				ver := BIN_PATH + "/Version"
				if err := copyAndChmod(verUpdate, ver); err != nil {
					os.Remove(WATCH_DIR + "/iptv")
					os.Remove(WATCH_DIR + "/Version")
					log.Printf("复制 管理系统版本文件 失败: %v", err)
					return false
				}
			}
			startIPTV()
		} else {
			os.Remove(WATCH_DIR + "/iptv")
			os.Remove(WATCH_DIR + "/Version")
			log.Println("管理系统 文件不存在，跳过更新")
			startIPTV()
		}
	}

	log.Println("更新完成")
	return true
}

func Exists(path string) bool {
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	return false
}

func ReadFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func copyAndChmod(src, dst string) error {
	input, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, input, 0755); err != nil { // 设置可执行权限
		return err
	}
	return nil
}

func isNewer(newVer, oldVer string, vLen int, boot bool) (bool, error) {
	if newVer == oldVer {
		return false, errors.New("已是最新版本，不更新")
	}
	newVer = strings.TrimPrefix(newVer, "v")
	oldVer = strings.TrimPrefix(oldVer, "v")

	if strings.Contains(newVer, "beta") && strings.Contains(oldVer, "beta") {
		newVer = strings.TrimSuffix(newVer, "beta")
		oldVer = strings.TrimSuffix(oldVer, "beta")
	} else if strings.Contains(newVer, "beta") {
		return false, errors.New("新版为beta版本，不更新")
	} else if strings.Contains(oldVer, "beta") {
		return false, errors.New("beta版本支持不更新")
	}

	np := strings.Split(newVer, ".")
	op := strings.Split(oldVer, ".")
	for len(np) < vLen {
		np = append(np, "0")
	}
	for len(op) < vLen {
		op = append(op, "0")
	}

	for i := 0; i < vLen; i++ {
		var a, b int
		fmt.Sscanf(np[i], "%d", &a)
		fmt.Sscanf(op[i], "%d", &b)
		if a > b {
			if i <= 1 && vLen == 4 && !boot {
				return false, errors.New("管理系统版本更新内容较大或基础镜像更新，不支持自动升级，请升级镜像")
			} else if i == 0 && vLen == 3 && !boot {
				return false, errors.New("引擎版本更新内容较大或基础镜像更新，不支持自动升级，请升级镜像")
			}
			return true, nil
		}
		if a < b {
			log.Println("新版本低于当前版本，不更新")
			return false, errors.New("low")
		}
	}
	return false, errors.New("版本号读取失败")
}
