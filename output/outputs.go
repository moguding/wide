// 构建、运行、go tool 操作.
package output

import (
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/b3log/wide/conf"
	"github.com/b3log/wide/session"
	"github.com/b3log/wide/util"
	"github.com/golang/glog"
	"github.com/gorilla/websocket"
)

// 输出通道.
// <sid, *util.WSChannel>
var outputWS = map[string]*util.WSChannel{}

// 建立输出通道.
func WSHandler(w http.ResponseWriter, r *http.Request) {
	// TODO: 会话校验
	sid := r.URL.Query()["sid"][0]

	conn, _ := websocket.Upgrade(w, r, nil, 1024, 1024)
	wsChan := util.WSChannel{Sid: sid, Conn: conn, Request: r, Time: time.Now()}

	outputWS[sid] = &wsChan

	ret := map[string]interface{}{"output": "Ouput initialized", "cmd": "init-output"}
	wsChan.Conn.WriteJSON(&ret)

	glog.V(4).Infof("Open a new [Output] with session [%s], %d", sid, len(outputWS))
}

// 运行一个可执行文件.
func RunHandler(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{"succ": true}
	defer util.RetJSON(w, r, data)

	decoder := json.NewDecoder(r.Body)

	var args map[string]interface{}

	if err := decoder.Decode(&args); err != nil {
		glog.Error(err)
		data["succ"] = false

		return
	}

	// TODO: 会话校验
	sid := args["sid"].(string)

	filePath := args["executable"].(string)
	curDir := filePath[:strings.LastIndex(filePath, string(os.PathSeparator))]

	cmd := exec.Command(filePath)
	cmd.Dir = curDir

	stdout, err := cmd.StdoutPipe()
	if nil != err {
		glog.Error(err)
		data["succ"] = false

		return
	}

	stderr, err := cmd.StderrPipe()
	if nil != err {
		glog.Error(err)
		data["succ"] = false

		return
	}

	reader := io.MultiReader(stdout, stderr)

	if err := cmd.Start(); nil != err {
		glog.Error(err)
		data["succ"] = false

		return
	}

	// 添加到用户进程集中
	processes.add(sid, cmd.Process)

	channelRet := map[string]interface{}{}
	channelRet["pid"] = cmd.Process.Pid

	go func(runningId int) {
		glog.V(3).Infof("Session [%s] is running [id=%d, file=%s]", sid, runningId, filePath)

		for {
			buf := make([]byte, 1024)
			count, err := reader.Read(buf)

			if nil != err || 0 == count {
				// 从用户进程集中移除这个执行完毕的进程
				processes.remove(sid, cmd.Process)

				glog.V(3).Infof("Session [%s] 's running [id=%d, file=%s] has done", sid, runningId, filePath)

				if nil != outputWS[sid] {
					wsChannel := outputWS[sid]

					channelRet["cmd"] = "run-done"
					channelRet["output"] = string(buf[:count])
					err := wsChannel.Conn.WriteJSON(&channelRet)
					if nil != err {
						glog.Error(err)
						break
					}

					// 更新通道最近使用时间
					wsChannel.Time = time.Now()
				}

				break
			} else {
				if nil != outputWS[sid] {
					wsChannel := outputWS[sid]

					channelRet["cmd"] = "run"
					channelRet["output"] = string(buf[:count])
					err := wsChannel.Conn.WriteJSON(&channelRet)
					if nil != err {
						glog.Error(err)
						break
					}

					// 更新通道最近使用时间
					wsChannel.Time = time.Now()
				}
			}
		}
	}(rand.Int())
}

// 构建可执行文件.
func BuildHandler(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{"succ": true}
	defer util.RetJSON(w, r, data)

	httpSession, _ := session.HTTPSession.Get(r, "wide-session")
	username := httpSession.Values["username"].(string)

	decoder := json.NewDecoder(r.Body)

	var args map[string]interface{}

	if err := decoder.Decode(&args); err != nil {
		glog.Error(err)
		data["succ"] = false

		return
	}

	// TODO: 会话校验
	sid := args["sid"].(string)

	filePath := args["file"].(string)
	curDir := filePath[:strings.LastIndex(filePath, string(os.PathSeparator))]

	fout, err := os.Create(filePath)

	if nil != err {
		glog.Error(err)
		data["succ"] = false

		return
	}

	code := args["code"].(string)

	fout.WriteString(code)

	if err := fout.Close(); nil != err {
		glog.Error(err)
		data["succ"] = false

		return
	}

	suffix := ""
	if "windows" == runtime.GOOS {
		suffix = ".exe"
	}
	executable := "main" + suffix
	argv := []string{"build", "-o", executable, filePath}

	cmd := exec.Command("go", argv...)
	cmd.Dir = curDir

	setCmdEnv(cmd, username)

	glog.V(5).Infof("go build -o %s %s", executable, filePath)

	executable = curDir + string(os.PathSeparator) + executable

	// 先把可执行文件删了
	err = os.RemoveAll(executable)
	if nil != err {
		glog.Info(err)
		data["succ"] = false

		return
	}

	stdout, err := cmd.StdoutPipe()
	if nil != err {
		glog.Error(err)
		data["succ"] = false

		return
	}

	stderr, err := cmd.StderrPipe()
	if nil != err {
		glog.Error(err)
		data["succ"] = false

		return
	}

	if data["succ"].(bool) {
		reader := io.MultiReader(stdout, stderr)

		if err := cmd.Start(); nil != err {
			glog.Error(err)
			data["succ"] = false

			return
		}

		go func(runningId int) {
			glog.V(3).Infof("Session [%s] is building [id=%d, file=%s]", sid, runningId, filePath)

			// 一次性读取
			buf := make([]byte, 1024*8)
			count, _ := reader.Read(buf)

			channelRet := map[string]interface{}{}
			channelRet["output"] = string(buf[:count])
			channelRet["cmd"] = "build"
			channelRet["executable"] = executable

			if 0 == count { // 说明构建成功，没有错误信息输出
				// 设置下一次执行命令（前端会根据这个发送请求）
				channelRet["nextCmd"] = "run"

				go func() { // 运行 go install，生成的库用于 gocode lib-path
					cmd := exec.Command("go", "install")
					cmd.Dir = curDir

					setCmdEnv(cmd, username)

					out, _ := cmd.CombinedOutput()
					if len(out) > 0 {
						glog.Warning(string(out))
					}
				}()
			} else { // 构建失败
				// 解析错误信息，返回给编辑器 gutter lint
				lines := strings.Split(string(buf[:count]), "\n")[1:]
				lints := []map[string]interface{}{}

				for _, line := range lines {
					if len(line) <= 1 {
						continue
					}

					file := line[:strings.Index(line, ":")]
					left := line[strings.Index(line, ":")+1:]
					lineNo, _ := strconv.Atoi(left[:strings.Index(left, ":")])
					msg := left[strings.Index(left, ":")+2:]

					lint := map[string]interface{}{}
					lint["file"] = file
					lint["lineNo"] = lineNo - 1
					lint["msg"] = msg
					lint["severity"] = "error" // warning
					lints = append(lints, lint)
				}

				channelRet["lints"] = lints
			}

			if nil != outputWS[sid] {
				glog.V(3).Infof("Session [%s] 's build [id=%d, file=%s] has done", sid, runningId, filePath)

				wsChannel := outputWS[sid]
				err := wsChannel.Conn.WriteJSON(&channelRet)
				if nil != err {
					glog.Error(err)
				}

				// 更新通道最近使用时间
				wsChannel.Time = time.Now()
			}

		}(rand.Int())
	}
}

// go install.
func GoInstallHandler(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{"succ": true}
	defer util.RetJSON(w, r, data)

	httpSession, _ := session.HTTPSession.Get(r, "wide-session")
	username := httpSession.Values["username"].(string)

	decoder := json.NewDecoder(r.Body)

	var args map[string]interface{}

	if err := decoder.Decode(&args); err != nil {
		glog.Error(err)
		data["succ"] = false

		return
	}

	// TODO: 会话校验
	sid := args["sid"].(string)

	filePath := args["file"].(string)
	curDir := filePath[:strings.LastIndex(filePath, string(os.PathSeparator))]

	fout, err := os.Create(filePath)

	if nil != err {
		glog.Error(err)
		data["succ"] = false

		return
	}

	code := args["code"].(string)

	fout.WriteString(code)

	if err := fout.Close(); nil != err {
		glog.Error(err)
		data["succ"] = false

		return
	}

	cmd := exec.Command("go", "install")
	cmd.Dir = curDir

	setCmdEnv(cmd, username)

	glog.V(5).Infof("go install %s", curDir)

	stdout, err := cmd.StdoutPipe()
	if nil != err {
		glog.Error(err)
		data["succ"] = false

		return
	}

	stderr, err := cmd.StderrPipe()
	if nil != err {
		glog.Error(err)
		data["succ"] = false

		return
	}

	if data["succ"].(bool) {
		reader := io.MultiReader(stdout, stderr)

		cmd.Start()

		go func(runningId int) {
			glog.V(3).Infof("Session [%s] is running [go install] [id=%d, dir=%s]", sid, runningId, curDir)

			// 一次性读取
			buf := make([]byte, 1024*8)
			count, _ := reader.Read(buf)

			channelRet := map[string]interface{}{}
			channelRet["output"] = string(buf[:count])
			channelRet["cmd"] = "go install"

			if 0 != count { // 构建失败
				// 解析错误信息，返回给编辑器 gutter lint
				lines := strings.Split(string(buf[:count]), "\n")[1:]
				lints := []map[string]interface{}{}

				for _, line := range lines {
					if len(line) <= 1 {
						continue
					}

					file := line[:strings.Index(line, ":")]
					left := line[strings.Index(line, ":")+1:]
					lineNo, _ := strconv.Atoi(left[:strings.Index(left, ":")])
					msg := left[strings.Index(left, ":")+2:]

					lint := map[string]interface{}{}
					lint["file"] = file
					lint["lineNo"] = lineNo - 1
					lint["msg"] = msg
					lint["severity"] = "error" // warning
					lints = append(lints, lint)
				}

				channelRet["lints"] = lints
			}

			if nil != outputWS[sid] {
				glog.V(3).Infof("Session [%s] 's running [go install] [id=%d, dir=%s] has done", sid, runningId, curDir)

				wsChannel := outputWS[sid]
				err := wsChannel.Conn.WriteJSON(&channelRet)
				if nil != err {
					glog.Error(err)
				}

				// 更新通道最近使用时间
				wsChannel.Time = time.Now()
			}

		}(rand.Int())
	}
}

// go get.
func GoGetHandler(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{"succ": true}
	defer util.RetJSON(w, r, data)

	httpSession, _ := session.HTTPSession.Get(r, "wide-session")
	username := httpSession.Values["username"].(string)

	decoder := json.NewDecoder(r.Body)

	var args map[string]interface{}

	if err := decoder.Decode(&args); err != nil {
		glog.Error(err)
		data["succ"] = false

		return
	}

	// TODO: 会话校验
	sid := args["sid"].(string)

	filePath := args["file"].(string)
	curDir := filePath[:strings.LastIndex(filePath, string(os.PathSeparator))]

	cmd := exec.Command("go", "get")
	cmd.Dir = curDir

	setCmdEnv(cmd, username)

	stdout, err := cmd.StdoutPipe()
	if nil != err {
		glog.Error(err)
		data["succ"] = false

		return
	}

	stderr, err := cmd.StderrPipe()
	if nil != err {
		glog.Error(err)
		data["succ"] = false

		return
	}

	reader := io.MultiReader(stdout, stderr)

	cmd.Start()

	channelRet := map[string]interface{}{}

	go func(runningId int) {
		glog.V(3).Infof("Session [%s] is running [go get] [runningId=%d]", sid, runningId)

		for {
			buf := make([]byte, 1024)
			count, err := reader.Read(buf)

			if nil != err || 0 == count {
				glog.V(3).Infof("Session [%s] 's running [go get] [runningId=%d] has done", sid, runningId)

				break
			} else {
				channelRet["output"] = string(buf[:count])
				channelRet["cmd"] = "go get"

				if nil != outputWS[sid] {
					wsChannel := outputWS[sid]

					err := wsChannel.Conn.WriteJSON(&channelRet)
					if nil != err {
						glog.Error(err)
						break
					}

					// 更新通道最近使用时间
					wsChannel.Time = time.Now()
				}
			}
		}
	}(rand.Int())
}

func setCmdEnv(cmd *exec.Cmd, username string) {
	userWorkspace := conf.Wide.GetUserWorkspace(username)

	cmd.Env = append(cmd.Env,
		"GOPATH="+userWorkspace,
		"GOOS="+runtime.GOOS,
		"GOARCH="+runtime.GOARCH,
		"GOROOT="+runtime.GOROOT(),
		"PATH="+os.Getenv("PATH"))
}
