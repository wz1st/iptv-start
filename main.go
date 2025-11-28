package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
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
	_ = LICENSE_CMD.Process.Kill()
	_ = LICENSE_CMD.Wait()
	LICENSE_CMD = nil
	if startLicense() {
		if waitLicense() {
			log.Println("授权服务重启成功")
			fmt.Fprintln(w, "OK")
			return
		}
	}
	fmt.Fprintln(w, "FAIL")
}

// 启动授权服务
func startLicense() bool {
	if LICENSE_CMD != nil {
		return true
	}
	log.Println("启动授权服务...")
	if !Exists(BIN_PATH + "/Version_lic") {
		os.Remove(BIN_PATH + "/license")
		os.Remove(BIN_PATH + "/Version_lic")
	}
	if !Exists(BIN_PATH + "/license") {
		if err := os.MkdirAll(BIN_PATH, 0755); err != nil {
			log.Fatal("/config/bin/创建目录失败,请检查目录权限并删除/config/bin/和/config/updata目录")
		}
		if err := copyAndChmod("/app/license", BIN_PATH+"/license"); err != nil {
			log.Fatal("复制文件license失败,请检查目录权限并删除/config/bin/和/config/updata目录")
		}
		if err := copyAndChmod("/app/Version_lic", BIN_PATH+"/Version_lic"); err != nil {
			log.Fatal("复制文件Version_lic失败,请检查目录权限并删除/config/bin/和/config/updata目录")
		}

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
	log.Printf("授权服务已启动，PID=%d", cmd.Process.Pid)
	return waitLicense()
}

// 等待授权服务可用 (通过 WebSocket ping)
func waitLicense() bool {
	url := "ws://127.0.0.1:81/ws"
	timeout := 60 * time.Second
	start := time.Now()
	log.Println("等待授权服务可用...")

	for {
		ws, err := websocket.Dial(url, "", "http://127.0.0.1:81/")
		if err == nil {
			// 发送 ping 消息
			msg := []byte("ping")
			if _, err := ws.Write(msg); err == nil {
				ws.Close()
				log.Println("授权服务可用")
				return true
			}
			ws.Close()
		}

		if time.Since(start) > timeout {
			log.Println("等待授权服务超时")
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
	if !Exists(BIN_PATH + "/Version") {
		os.Remove(BIN_PATH + "/iptv")
		os.Remove(BIN_PATH + "/Version")
	}
	if !Exists(BIN_PATH + "/iptv") {
		if err := os.MkdirAll(BIN_PATH, 0755); err != nil {
			log.Fatal("/config/bin/ 创建目录失败,请检查目录权限并删除/config/bin/和/config/updata目录")
		}
		if err := copyAndChmod("/app/iptv", BIN_PATH+"/iptv"); err != nil {
			log.Fatal("复制文件iptv失败,请检查目录权限并删除/config/bin/和/config/updata目录")
		}
		if err := copyAndChmod("/app/Version", BIN_PATH+"/Version"); err != nil {
			log.Fatal("复制文件Version失败,请检查目录权限并删除/config/bin/和/config/updata目录")
		}
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
		// 判断版本
		newLicVersion := ReadFile("/app/Version_lic")
		curLicVersion := ReadFile(BIN_PATH + "/Version_lic")
		if curLicVersion != "" {
			if curLicVersion == "local" {
				log.Println("授权服务为本地版本，跳过")
				startLicense()
			}
			switch newLicVersion {
			case "":
				log.Println("授权服务版本文件不存在，跳过")
				if LICENSE_CMD == nil {
					startLicense()
				}

			case curLicVersion:
				log.Println("授权服务为最新版本，跳过更新")
				if LICENSE_CMD == nil {
					startLicense()
				}
			default:
				check, err := isNewer(newLicVersion, curLicVersion, 3)
				if err != nil {
					log.Println(err.Error())
					if LICENSE_CMD == nil {
						startLicense()
					}
				}
				if check {
					licUp = true
				}
			}
		} else {
			licUp = true
		}

		if licUp {
			// 更新 license
			license := "/app/license"
			if _, err := os.Stat(license); err == nil {
				log.Println("复制 license...")
				if LICENSE_CMD != nil {
					_ = LICENSE_CMD.Process.Kill()
					_ = LICENSE_CMD.Wait()
					LICENSE_CMD = nil
				}
				if err := os.MkdirAll(BIN_PATH, 0755); err != nil {
					log.Fatalln("复制启动文件失败，请检查目录权限")
				}
				dst := BIN_PATH + "/license"
				if err := copyAndChmod(license, dst); err != nil {
					log.Fatalf("复制 license 失败: %v", err)
				}
				ver := BIN_PATH + "/Version_lic"
				if err := copyAndChmod("/app/Version_lic", ver); err != nil {
					log.Fatalf("复制 Version_lic 失败: %v", err)
				}
				startLicense()

			} else {
				log.Fatalln("/app目录中 授权服务文件不存在")
			}
		} else {
			startLicense()
		}

		newWebVersion := ReadFile("/app/Version")
		curWebVersion := ReadFile(BIN_PATH + "/Version")
		if curWebVersion != "" {
			if curWebVersion == "local" {
				log.Println("管理系统为本地版本，跳过更新")
				return startIPTV()
			}
			switch newWebVersion {
			case "":
				log.Println("管理系统版本文件不存在，跳过")
				if IPTV_CMD == nil {
					return startIPTV()
				}

			case curWebVersion:
				log.Println("管理系统为最新版本，跳过更新")
				if IPTV_CMD == nil {
					return startIPTV()
				}
			default:
				check, err := isNewer(newWebVersion, curWebVersion, 4)
				if err != nil {
					log.Println(err.Error())
					if IPTV_CMD == nil {
						return startIPTV()
					}
				}
				if check {
					webUp = true
				}
			}
		} else {
			webUp = true
		}

		if webUp {
			// 更新 IPTV
			iptv := "/app/iptv"
			if _, err := os.Stat(iptv); err == nil {
				log.Println("复制 IPTV...")
				if IPTV_CMD != nil {
					_ = IPTV_CMD.Process.Kill()
					_ = IPTV_CMD.Wait()
					IPTV_CMD = nil
				}
				if err := os.MkdirAll(BIN_PATH, 0755); err != nil {
					log.Fatalln("复制启动文件失败，请检查目录权限  ,请检查目录权限并删除/config/bin/和/config/updata目录")
				}
				dst := BIN_PATH + "/iptv"
				if err := copyAndChmod(iptv, dst); err != nil {
					log.Fatalf("复制 IPTV 失败: %v    ,请检查目录权限并删除/config/bin/和/config/updata目录", err)
				}
				ver := BIN_PATH + "/Version"
				if err := copyAndChmod("/app/Version", ver); err != nil {
					log.Fatalf("复制 Version 失败: %v    ,请检查目录权限并删除/config/bin/和/config/updata目录", err)
				}
				return startIPTV()

			} else {
				log.Fatalln("/app目录中 管理系统文件不存在")
			}
		} else {
			return startIPTV()
		}
	}
	if _, err := os.Stat(WATCH_DIR); os.IsNotExist(err) {
		log.Println("更新目录不存在，跳过")
		if LICENSE_CMD == nil {
			startLicense()
		}
		if IPTV_CMD == nil {
			startIPTV()
		}
		return false
	}

	if (!Exists(WATCH_DIR+"/Version") || !Exists(WATCH_DIR+"/iptv")) &&
		(!Exists(WATCH_DIR+"/Version_lic") || !Exists(WATCH_DIR+"/license")) {
		log.Println("更新文件不完整，跳过")
		if LICENSE_CMD == nil {
			startLicense()
		}
		if IPTV_CMD == nil {
			startIPTV()
		}
		return false
	}

	log.Println("开始更新")

	licUp := false
	webUp := false

	// 判断版本
	newLicVersion := ReadFile(WATCH_DIR + "/Version_lic")
	curLicVersion := ReadFile(BIN_PATH + "/Version_lic")

	switch newLicVersion {
	case "":
		log.Println("授权服务版本文件不存在，跳过")
		if LICENSE_CMD == nil {
			startLicense()
		}

	case curLicVersion:
		log.Println("授权服务为最新版本，跳过更新")
		if LICENSE_CMD == nil {
			startLicense()
		}
	default:
		check, err := isNewer(newLicVersion, curLicVersion, 3)
		if err != nil {
			log.Println(err.Error())
			if LICENSE_CMD == nil {
				startLicense()
			}
		}
		if check {
			licUp = true
		}
	}

	if licUp {
		// 更新 license
		license := WATCH_DIR + "/license"
		verUpdate := WATCH_DIR + "/Version_lic"
		if _, err := os.Stat(license); err == nil {
			log.Println("更新 license...")

			if err := os.MkdirAll(BIN_PATH, 0755); err != nil {
				log.Fatalln("复制启动文件失败，请检查目录权限")
			}
			ver := BIN_PATH + "/Version_lic"
			if err := copyAndChmod(verUpdate, ver); err != nil {
				log.Printf("复制 Version_lic 失败: %v   ,请检查目录权限并删除/config/bin/和/config/updata目录", err)
				return false
			}

			if LICENSE_CMD != nil {
				_ = LICENSE_CMD.Process.Kill()
				_ = LICENSE_CMD.Wait()
				LICENSE_CMD = nil
			}
			dst := BIN_PATH + "/license"
			if err := copyAndChmod(license, dst); err != nil {
				log.Printf("复制 license 失败: %v  ,请检查目录权限并删除/config/bin/和/config/updata目录", err)
			} else {
				startLicense()
			}
		} else {
			log.Println("授权服务文件不存在，跳过更新")
			if LICENSE_CMD == nil {
				startLicense()
			}

		}
	} else {
		if LICENSE_CMD == nil {
			startLicense()
		}
	}

	newWebVersion := ReadFile(WATCH_DIR + "/Version")
	curWebVersion := ReadFile(BIN_PATH + "/Version")
	switch newWebVersion {
	case "":
		log.Println("管理系统版本文件不存在，跳过")
		if IPTV_CMD == nil {
			return startIPTV()
		}

	case curWebVersion:
		log.Println("管理系统为最新版本，跳过更新")
		if IPTV_CMD == nil {
			return startIPTV()
		}
	default:
		check, err := isNewer(newWebVersion, curWebVersion, 4)
		if err != nil {
			log.Println(err.Error())
			if IPTV_CMD == nil {
				return startIPTV()
			}
		}
		if check {
			webUp = true
		}
	}

	if webUp {
		// 更新 IPTV
		iptv := WATCH_DIR + "/iptv"
		verUpdate := WATCH_DIR + "/Version"
		if _, err := os.Stat(iptv); err == nil {
			log.Println("更新 IPTV...")

			if err := os.MkdirAll(BIN_PATH, 0755); err != nil {
				log.Fatalln("复制启动文件失败，请检查目录权限 ,请检查目录权限并删除/config/bin/和/config/updata目录")
			}
			ver := BIN_PATH + "/Version"
			if err := copyAndChmod(verUpdate, ver); err != nil {
				log.Printf("复制 Version 失败: %v    ,请检查目录权限并删除/config/bin/和/config/updata目录", err)
				return false
			}

			if IPTV_CMD != nil {
				_ = IPTV_CMD.Process.Kill()
				_ = IPTV_CMD.Wait()
				IPTV_CMD = nil
			}
			dst := BIN_PATH + "/iptv"
			if err := copyAndChmod(iptv, dst); err != nil {
				log.Printf("复制 IPTV 失败: %v    ,请检查目录权限并删除/config/bin/和/config/updata目录", err)
			} else {
				return startIPTV()
			}
		} else {
			log.Println("IPTV 文件不存在，跳过更新")
			if IPTV_CMD == nil {
				return startIPTV()
			}
		}
	} else {
		if IPTV_CMD == nil {
			return startIPTV()
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

func isNewer(newVer, oldVer string, vLen int) (bool, error) {
	if newVer == oldVer {
		return false, errors.New("已是最新版本，不更新")
	}
	newVer = strings.TrimPrefix(newVer, "v")
	oldVer = strings.TrimPrefix(oldVer, "v")

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
			if i <= 1 && vLen == 4 {
				return false, errors.New("管理系统版本更新内容较大或基础镜像更新，不支持自动升级，请升级镜像")
			} else if i == 0 && vLen == 3 {
				return false, errors.New("授权服务版本更新内容较大或基础镜像更新，不支持自动升级，请升级镜像")
			}
			return true, nil
		}
		if a < b {
			return false, errors.New("新版本低于当前版本，不更新")
		}
	}
	return false, errors.New("版本号读取失败")
}
