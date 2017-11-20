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
	respRoom    = "room"
	respEnd     = "end"
)

var (
	// Address of the server
	Address string
	// Port of the server
	Port     string
	err      error
	exitCode int
	// json configuration
	Conf Configuration
	// websocket
	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     func(r *http.Request) bool { return true },
	}
	wsconn *websocket.Conn
	wserr  error

	// winrm parameters
	winrmPort     int
	winrmHTTPS    bool
	winrmInsecure bool
	winrmTimeout  time.Duration
	winrmUser     string
	winrmPass     string
	// ssh parameters
	sshTimeout time.Duration
	sshPort    int
	sshPem     string

	// Resp is passed to the view
	Resp Response

	// winrm and ssh connections
	winrmClient *winrm.Client
	sshClient   *ssh.Client
	sshSession  *ssh.Session
)

// json configuration
type Configuration struct {
	WinrmPort     int           `json:"winrmPort"`
	WinrmHTTPS    bool          `json:"winmlHTTPS"`
	WinrmInsecure bool          `json:"winrmInsecure"`
	WinrmTimeout  time.Duration `json:"winrmTimeout"`
	WinrmUser     string        `json:"winrmUser"`
	WinrmPass     string        `json:"winrmPass"`
	SshTimeout    time.Duration `json:"sshTimeout"`
	SshPort       int           `json:"sshPort"`
	SshPem        string        `json:"sshPem"`
	Dpts          []DptC        `json:"dpts"`
}
type DptC struct {
	Title    string     `json:"title"`
	Machines []MachineC `json:"machines"`
}
type MachineC struct {
	Name string `json:"name"`
	Nb   int    `json:"nb"`
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

// Param is passed to the template
type Param struct {
	Address string
	Port    string
	Conf    *Configuration
}

// Response is a structure sent to the view by JSON
type Response struct {
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
	}

	key, err := ssh.ParsePrivateKey(buffer)
	if err != nil {
		fmt.Printf("error parsing ssh private key file: %s", err.Error)
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

		// searching for the room nb of machines
		nbmach := 0
		for _, d := range Conf.Dpts {
			for _, m := range d.Machines {
				if m.Name == room.Name {
					nbmach = m.Nb
					break
				}
			}
		}

		// looping throught the machines
		for i := 01; i <= nbmach; i++ {

			go func(machine int, room string, wss *WSReaderWriter) {

				// building the cname
				cname := room + fmt.Sprintf("%02d", machine)
				// removing iutcl for the display name
				dname := cname[8:]
				fmt.Printf("machine %s\n", cname)
				// trying a ping
				fmt.Printf("  ping test\n")
				//var cmdOut []byte
				if _, err = exec.Command("ping", "-c 1", "-W 1", cname).Output(); err != nil {
					fmt.Printf("  -> no ping\n")
					// returning the JSON
					Resp = Response{Type: respMachine, Mach: Machine{Name: dname, OS: "down", Room: room}}
					myJSON, err := json.Marshal(Resp)
					if err != nil {
						fmt.Println("json marshal error " + err.Error())
						return
					}
					wss.Send(myJSON)
					return
				}
				//fmt.Println(cmdOut)

				// trying a windows connection
				fmt.Printf("  windows test\n")
				endpoint := winrm.NewEndpoint(cname, winrmPort, winrmHTTPS, winrmInsecure, nil, nil, nil, winrmTimeout)
				if winrmClient, err = winrm.NewClient(endpoint, winrmUser, winrmPass); err != nil {
					panic(err)
				}

				// trying a simple dir command
				//if exitCode, err = winrmClient.Run("dir", os.Stdout, os.Stderr); err != nil {
				if exitCode, err = winrmClient.Run("dir", ioutil.Discard, os.Stderr); err != nil {
					// machine under Linux or down
					fmt.Printf("  -> not windows\n")
				} else if exitCode == 0 {
					// machine under windows
					// returning the JSON
					Resp = Response{Type: respMachine, Mach: Machine{Name: dname, OS: "windows", Room: room}}
					myJSON, err := json.Marshal(Resp)
					if err != nil {
						fmt.Println("json marshal error " + err.Error())
						return
					}
					wss.Send(myJSON)
					fmt.Printf("  -> windows\n")
					return
				}

				// trying a linux connection
				fmt.Printf("  linux test\n")
				sshConfig := &ssh.ClientConfig{
					User: "root",
					Auth: []ssh.AuthMethod{
						PublicKeyFile("./id_rsa.pem"),
					},
					Timeout: sshTimeout,
					HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
						return nil
					},
				}
				if sshClient, err = ssh.Dial("tcp", fmt.Sprintf("%s:%d", cname, sshPort), sshConfig); err != nil {
					// can not dial - machine unknown
					// returning the JSON
					Resp = Response{Type: respMachine, Mach: Machine{Name: dname, OS: "unknown", Room: room}}
					myJSON, err := json.Marshal(Resp)
					if err != nil {
						fmt.Println("json marshal error " + err.Error())
						return
					}
					wss.Send(myJSON)
					fmt.Printf("  -> not linux (ssh dial error) " + err.Error() + "\n")
					return
				}
				if sshSession, err = sshClient.NewSession(); err != nil {
					// returning the JSON
					Resp = Response{Type: respMachine, Mach: Machine{Name: dname, OS: "unknown", Room: room}}
					myJSON, err := json.Marshal(Resp)
					if err != nil {
						fmt.Println("json marshal error " + err.Error())
						return
					}
					wss.Send(myJSON)
					fmt.Printf("  -> not linux (ssh session error) " + err.Error() + "\n")
					return
				}
				if err = sshSession.Run("ls"); err != nil {
					// can not run command - machine probably under Linux
					// returning the JSON
					Resp = Response{Type: respMachine, Mach: Machine{Name: dname, OS: "unknown", Room: room}}
					myJSON, err := json.Marshal(Resp)
					if err != nil {
						fmt.Println("json marshal error " + err.Error())
						return
					}
					wss.Send(myJSON)
					fmt.Printf("  -> not linux (ssh remote command error) " + err.Error() + "\n")
					return
				}
				// returning the JSON
				Resp = Response{Type: respMachine, Mach: Machine{Name: dname, OS: "linux", Room: room}}
				myJSON, err := json.Marshal(Resp)
				if err != nil {
					fmt.Println("json marshal error " + err.Error())
					return
				}
				wss.Send(myJSON)
				fmt.Printf("  -> linux\n")
			}(i, room.Name, wss)
		}
	}
}

func MainHandler(w http.ResponseWriter, r *http.Request) {
	staticBox := rice.MustFindBox("static")
	p := Param{Address: Address, Port: Port, Conf: &Conf}

	// Building the HTML template.
	htmlTplString, err := staticBox.String("index.html")
	htmlTmp, err := template.New("index").Parse(htmlTplString)
	if err != nil {
		log.Fatal(err)
	}
	err = htmlTmp.Execute(w, p)
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

	winrmPort = Conf.WinrmPort
	winrmHTTPS = Conf.WinrmHTTPS
	winrmInsecure = Conf.WinrmInsecure
	winrmTimeout = Conf.WinrmTimeout
	winrmUser = Conf.WinrmUser
	winrmPass = Conf.WinrmPass
	sshTimeout = Conf.SshTimeout
	sshPort = Conf.SshPort
	sshPem = Conf.SshPem

}

func main() {

	// Getting the program parameters.
	address := flag.String("address", "localhost", "server address")
	port := flag.String("port", "8080", "the port to listen")
	flag.Parse()
	Address = *address
	Port = *port

	fmt.Printf("address: %s port: %s", Address, Port)

	cssBox := rice.MustFindBox("static/css")
	cssFileServer := http.StripPrefix("/css/", http.FileServer(cssBox.HTTPBox()))

	http.Handle("/css/", cssFileServer)
	http.HandleFunc("/socket/", SocketHandler)
	http.HandleFunc("/", MainHandler)

	if err = http.ListenAndServe(Address+":"+Port, nil); err != nil {
		panic(err)
	}
}