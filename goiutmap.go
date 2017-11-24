package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	rice "github.com/GeertJohan/go.rice"
	"github.com/gorilla/websocket"
	"github.com/masterzen/winrm"
	"golang.org/x/crypto/ssh"
)

const (
	// message type transmitted to the view
	respMachine = "machine"
)

var (
	err      error
	exitCode int
	// Conf is the json configuration
	Conf Config
	// websocket
	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     func(r *http.Request) bool { return true },
	}
	wsconn *websocket.Conn
	wserr  error

	// Resp is passed to the view
	Resp WSResp
)

// json configuration
type Config struct {
	Main ConfigMain   `json:"main"`
	Dpts []*ConfigDpt `json:"dpts"`
}
type ConfigMain struct {
	Address string // not in conf file, set up in init
	Port    string // not in conf file, set up in init
}
type ConfigDpt struct {
	Title      string        `json:"title"`
	Connection ConfigConn    `json:"connection"`
	Machines   []*ConfigMach `json:"machines"`
}
type ConfigConn struct {
	WinrmPort     int               `json:"winrmPort"`
	WinrmHTTPS    bool              `json:"winrmHTTPS"`
	WinrmInsecure bool              `json:"winrmInsecure"`
	WinrmTimeout  time.Duration     `json:"winrmTimeout"`
	WinrmUser     string            `json:"winrmUser"`
	WinrmPass     string            `json:"winrmPass"`
	SshTimeout    time.Duration     `json:"sshTimeout"`
	SshPort       int               `json:"sshPort"`
	SshUser       string            `json:"sshUser"`
	SshPem        string            `json:"sshPem"`
	SshConfig     *ssh.ClientConfig // not in conf file, set up in init
}
type ConfigMach struct {
	Name  string   `json:"name"`
	Nb    int      `json:"nb"`
	Range []string // not in conf file, set up in init
}

// Machine is a structure representing a machine
type Machine struct {
	Name string `json:"name"`
	OS   string `json:"os"`
	Room string `json:"room"`
}

// Room is a structure representing a room
type Room struct {
	Name   string `json:"name"`
	NbMach int    `json:"nbmach"`
}

// WSResp is a structure sent to the view by JSON by web socket
type WSResp struct {
	Type string `json:"type"`
	Mach Machine
}

// WSReaderWriter is a mutex websocket writer
type WSReaderWriter struct {
	ws  *websocket.Conn
	mux sync.Mutex
}

// Send writes the json to the websocket
func (wss *WSReaderWriter) Send(json []byte) {
	wss.mux.Lock()
	err = wss.ws.WriteMessage(websocket.TextMessage, json)
	if err != nil {
		fmt.Println(err)
	}
	wss.mux.Unlock()
}

// Read reads the json from the websocket
func (wss *WSReaderWriter) Read() *Room {
	r := Room{}
	err := wss.ws.ReadJSON(&r)
	if err != nil {
		fmt.Println(err)
	}
	return &r
}

// WSReaderWriter constructor
func NewWSsender(w http.ResponseWriter, r *http.Request) *WSReaderWriter {
	// opening the websocket
	wss, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		panic("error opening socket")
	}
	return &WSReaderWriter{ws: wss, mux: sync.Mutex{}}
}

// SSH key loader
func PublicKeyFile(file string) ssh.AuthMethod {
	buffer, err := ioutil.ReadFile(file)
	if err != nil {
		fmt.Printf("error reading ssh private key file: %s", err.Error)
		panic(err)
	}

	key, err := ssh.ParsePrivateKey(buffer)
	if err != nil {
		fmt.Printf("error parsing ssh private key file: %s", err.Error)
		panic(err)
	}
	return ssh.PublicKeys(key)
}

// SocketHandler handles the websocket communications
func SocketHandler(w http.ResponseWriter, r *http.Request) {
	// opening the websocket
	wss := NewWSsender(w, r)

	for {
		// receive the room to load/reload with name like infa12
		room := wss.Read()

		// searching for the room nb of machines and connection info
		nbmach := 0
		var conn ConfigConn

		for _, d := range Conf.Dpts {
			for _, m := range d.Machines {
				if m.Name == room.Name {
					nbmach = m.Nb
					conn = d.Connection
					break
				}
			}
		}

		// looping throught the machines
		for i := 01; i <= nbmach; i++ {

			go func(machine int, room string, conn *ConfigConn, wss *WSReaderWriter) {
				// winrm and ssh connections
				var (
					winrmClient *winrm.Client
					sshClient   *ssh.Client
					sshSession  *ssh.Session
					err         error
				)

				// building the cname
				cname := room + fmt.Sprintf("%02d", machine)
				fmt.Printf("machine %s\n", cname)
				// trying a ping
				fmt.Printf("  ping test\n")
				//var cmdOut []byte
				if _, err = exec.Command("ping", "-c 1", "-W 1", cname).Output(); err != nil {
					fmt.Printf("  -> no ping\n")
					// returning the JSON
					Resp = WSResp{Type: respMachine, Mach: Machine{Name: cname, OS: "down", Room: room}}
					myJSON, jerr := json.Marshal(Resp)
					if jerr != nil {
						fmt.Println("json marshal error " + jerr.Error())
						return
					}
					wss.Send(myJSON)
					return
				}
				//fmt.Println(cmdOut)

				// trying a windows connection
				fmt.Printf("  windows test\n")
				endpoint := winrm.NewEndpoint(cname, conn.WinrmPort, conn.WinrmHTTPS, conn.WinrmInsecure, nil, nil, nil, conn.WinrmTimeout)
				if winrmClient, err = winrm.NewClient(endpoint, conn.WinrmUser, conn.WinrmPass); err != nil {
					panic(err)
				}

				// trying a simple dir command
				//if exitCode, err = winrmClient.Run("dir", os.Stdout, os.Stderr); err != nil {
				if exitCode, err = winrmClient.Run("dir", ioutil.Discard, os.Stderr); err != nil {
					// machine under Linux or down
					fmt.Printf("  -> not windows " + err.Error() + "\n")
				} else if exitCode == 0 {
					// machine under windows
					// returning the JSON
					Resp = WSResp{Type: respMachine, Mach: Machine{Name: cname, OS: "windows", Room: room}}
					myJSON, jerr := json.Marshal(Resp)
					if jerr != nil {
						fmt.Println("json marshal error " + jerr.Error())
						return
					}
					wss.Send(myJSON)
					fmt.Printf("  -> windows\n")
					return
				}

				// trying a linux connection
				if sshClient, err = ssh.Dial("tcp", fmt.Sprintf("%s:%d", cname, conn.SshPort), conn.SshConfig); err != nil {
					// can not dial - machine unknown
					// returning the JSON
					Resp = WSResp{Type: respMachine, Mach: Machine{Name: cname, OS: "unknown", Room: room}}
					myJSON, jerr := json.Marshal(Resp)
					if jerr != nil {
						fmt.Println("json marshal error " + jerr.Error())
						return
					}
					wss.Send(myJSON)
					fmt.Printf("  -> not linux (ssh dial error) " + err.Error() + "\n")
					return
				}
				if sshSession, err = sshClient.NewSession(); err != nil {
					// returning the JSON
					Resp = WSResp{Type: respMachine, Mach: Machine{Name: cname, OS: "unknown", Room: room}}
					myJSON, jerr := json.Marshal(Resp)
					if jerr != nil {
						fmt.Println("json marshal error " + jerr.Error())
						return
					}
					wss.Send(myJSON)
					fmt.Printf("  -> not linux (ssh session error) " + err.Error() + "\n")
					return
				}
				if err = sshSession.Run("ls"); err != nil {
					// can not run command - machine probably under Linux
					// returning the JSON
					Resp = WSResp{Type: respMachine, Mach: Machine{Name: cname, OS: "unknown", Room: room}}
					myJSON, jerr := json.Marshal(Resp)
					if jerr != nil {
						fmt.Println("json marshal error " + jerr.Error())
						return
					}
					wss.Send(myJSON)
					fmt.Printf("  -> not linux (ssh remote command error) " + err.Error() + "\n")
					return
				}
				// returning the JSON
				Resp = WSResp{Type: respMachine, Mach: Machine{Name: cname, OS: "linux", Room: room}}
				myJSON, jerr := json.Marshal(Resp)
				if jerr != nil {
					fmt.Println("json marshal error " + jerr.Error())
					return
				}
				wss.Send(myJSON)
				fmt.Printf("  -> linux\n")
			}(i, room.Name, &conn, wss)
		}
	}
}

func MainHandler(w http.ResponseWriter, r *http.Request) {
	staticBox := rice.MustFindBox("static")

	// Building the HTML template.
	htmlTplString, err := staticBox.String("index.html")
	htmlTmp, err := template.New("index").Parse(htmlTplString)
	if err != nil {
		log.Fatal(err)
	}
	err = htmlTmp.Execute(w, Conf)
	if err != nil {
		log.Fatal(err)
	}
}

func init() {

	// loading json configuration file
	var (
		cf *os.File
	)
	if cf, err = os.Open("./configuration.json"); err != nil {
		panic(err)
	}
	decoder := json.NewDecoder(cf)
	if err = decoder.Decode(&Conf); err != nil {
		panic(err)
	}

	// initializing machine ranges
	// and departments SshConfig
	for _, d := range Conf.Dpts {

		d.Connection.SshConfig = &ssh.ClientConfig{
			User: d.Connection.SshUser,
			Auth: []ssh.AuthMethod{
				PublicKeyFile(d.Connection.SshPem),
			},
			Timeout: d.Connection.SshTimeout,
			HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
				return nil
			},
		}

		for _, m := range d.Machines {
			for i := 1; i <= m.Nb; i++ {
				m.Range = append(m.Range, fmt.Sprintf("%02d", i))
			}
		}
	}
}

func main() {

	// Getting the program parameters.
	address := flag.String("address", "localhost", "server address")
	port := flag.String("port", "8080", "the port to listen")
	flag.Parse()
	Conf.Main.Address = *address
	Conf.Main.Port = *port

	fmt.Printf("address: %s port: %s", Conf.Main.Address, Conf.Main.Port)

	cssBox := rice.MustFindBox("static/css")
	cssFileServer := http.StripPrefix("/css/", http.FileServer(cssBox.HTTPBox()))
	jsBox := rice.MustFindBox("static/js")
	jsFileServer := http.StripPrefix("/js/", http.FileServer(jsBox.HTTPBox()))

	http.Handle("/css/", cssFileServer)
	http.Handle("/js/", jsFileServer)
	http.HandleFunc("/socket/", SocketHandler)
	http.HandleFunc("/", MainHandler)

	if err = http.ListenAndServe(Conf.Main.Address+":"+Conf.Main.Port, nil); err != nil {
		panic(err)
	}
}
