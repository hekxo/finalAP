package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type Message struct {
	from    string
	payload []byte
}

type UserStatus struct {
	IP       string
	Typing   bool
	Offline  bool
	LastSeen time.Time
}

type Server struct {
	listenAddr   string
	ln           net.Listener
	quitch       chan struct{}
	msgch        chan Message
	logFile      *os.File
	connections  map[net.Conn]string
	ipToConn     map[string]net.Conn
	userStatus   map[string]*UserStatus
	bannedUsers  map[string]bool
	mu           sync.Mutex
	ipCounter    int
	simulatedIPs []string
	reuseIP      string
	reuseEnabled bool
	certFile     string
	keyFile      string
}

func NewServer(listenAddr, certFile, keyFile string) *Server {
	logFile, err := os.OpenFile("../data.txt", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatal(err)
	}

	simulatedIPs := []string{
		"192.168.1.2",
		"192.168.1.3",
	}

	return &Server{
		listenAddr:   listenAddr,
		quitch:       make(chan struct{}),
		msgch:        make(chan Message, 10),
		logFile:      logFile,
		connections:  make(map[net.Conn]string),
		ipToConn:     make(map[string]net.Conn),
		userStatus:   make(map[string]*UserStatus),
		bannedUsers:  make(map[string]bool),
		ipCounter:    0,
		simulatedIPs: simulatedIPs,
		certFile:     certFile,
		keyFile:      keyFile,
	}
}

func (s *Server) logMessage(message Message) {
	timestamp := time.Now().Format(time.RFC3339)
	logEntry := fmt.Sprintf("Your message is: %s. Received time: %s\n", string(message.payload), timestamp)

	fmt.Printf("Logging message: %s\n", logEntry)

	if _, err := s.logFile.WriteString(logEntry); err != nil {
		log.Printf("Error writing to log file: %v", err)
	}
}

func (s *Server) Start() error {
	cer, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		return fmt.Errorf("failed to load key pair: %v", err)
	}

	config := &tls.Config{Certificates: []tls.Certificate{cer}}
	ln, err := tls.Listen("tcp", s.listenAddr, config)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %v", s.listenAddr, err)
	}
	defer ln.Close()
	s.ln = ln

	go s.acceptLoop()

	<-s.quitch
	close(s.msgch)
	s.logFile.Close()

	return nil
}

func (s *Server) StartAdminPanel() {
	http.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()

		stats := "Server stats:\n"
		stats += "Connected users:\n"
		for conn, ip := range s.connections {
			stats += fmt.Sprintf("- %s (%s)\n", conn.RemoteAddr().String(), ip)
		}

		stats += "Banned users:\n"
		for ip := range s.bannedUsers {
			stats += fmt.Sprintf("- %s\n", ip)
		}

		stats += "User statuses:\n"
		for ip, status := range s.userStatus {
			stats += fmt.Sprintf("- %s: typing=%t, offline=%t, last seen=%s\n", ip, status.Typing, status.Offline, status.LastSeen)
		}

		w.Write([]byte(stats))
	})

	log.Fatal(http.ListenAndServe(":8080", nil))
}

func (s *Server) handleCommand(conn net.Conn, cmd string) {
	cmd = strings.TrimSpace(cmd)
	parts := strings.Split(cmd, " ")
	switch parts[0] {
	case "/help":
		conn.Write([]byte("Available commands: /help, /ban, /kick, /toggle_reuse, /join, /typing, /offline\n"))
	case "/ban":
		if len(parts) > 1 {
			s.banUser(parts[1])
			conn.Write([]byte("User has been banned.\n"))
		} else {
			conn.Write([]byte("Usage: /ban <user_ip>\n"))
		}
	case "/kick":
		if len(parts) > 1 {
			s.kickUser(parts[1])
			conn.Write([]byte("User has been kicked.\n"))
		} else {
			conn.Write([]byte("Usage: /kick <user_ip>\n"))
		}
	case "/toggle_reuse":
		s.mu.Lock()
		s.reuseEnabled = !s.reuseEnabled
		s.mu.Unlock()
		conn.Write([]byte(fmt.Sprintf("IP reuse set to %t.\n", s.reuseEnabled)))
	case "/typing":
		s.mu.Lock()
		ip := s.connections[conn]
		s.userStatus[ip].Typing = true
		s.mu.Unlock()
		s.broadcastStatus(ip, "is typing")
	case "/offline":
		s.mu.Lock()
		ip := s.connections[conn]
		s.userStatus[ip].Offline = true
		s.userStatus[ip].LastSeen = time.Now()
		s.mu.Unlock()
		s.broadcastStatus(ip, "is offline")
	case "/join":
		fmt.Printf("User %s joined the chat.\n", conn.RemoteAddr())
		conn.Write([]byte("Welcome to the chat!\n"))
	default:
		conn.Write([]byte("Unknown command.\n"))
	}
}

func (s *Server) banUser(userIP string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	log.Printf("Banning user %s...\n", userIP)
	s.bannedUsers[userIP] = true
	if conn, ok := s.ipToConn[userIP]; ok {
		conn.Write([]byte("You have been banned.\n"))
		conn.Close()
		delete(s.connections, conn)
		delete(s.ipToConn, userIP)
		delete(s.userStatus, userIP)
	}
	log.Printf("User %s has been banned.\n", userIP)
}

func (s *Server) kickUser(userIP string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	conn, ok := s.ipToConn[userIP]
	if !ok {
		log.Printf("User %s not found in ipToConn map.\n", userIP)
		return
	}

	log.Printf("Kicking user %s...\n", userIP)
	conn.Write([]byte("You have been kicked.\n"))
	conn.Close()

	delete(s.connections, conn)
	delete(s.ipToConn, userIP)
	delete(s.userStatus, userIP)
	log.Printf("User %s has been kicked and removed from maps.\n", userIP)
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			fmt.Println("accept error:", err)
			continue
		}

		s.mu.Lock()
		userIP := s.simulatedIPs[s.ipCounter%len(s.simulatedIPs)]
		s.ipCounter++

		if s.bannedUsers[userIP] {
			conn.Write([]byte("You are banned from this server.\n"))
			conn.Close()
			s.mu.Unlock()
			log.Printf("Banned user %s attempted to connect and was refused.\n", userIP)
			continue
		}

		s.connections[conn] = userIP
		s.ipToConn[userIP] = conn
		s.userStatus[userIP] = &UserStatus{IP: userIP, Typing: false, Offline: false, LastSeen: time.Now()}
		s.mu.Unlock()

		s.broadcastStatus(userIP, "has connected")
		fmt.Printf("New connection from IP: %s\n", userIP)
		log.Printf("New connection from IP: %s\n", userIP)

		go s.readLoop(conn, userIP)
	}
}

func (s *Server) readLoop(conn net.Conn, userIP string) {
	defer func() {
		s.mu.Lock()
		conn.Close()
		delete(s.connections, conn)
		delete(s.ipToConn, userIP)
		delete(s.userStatus, userIP)
		s.mu.Unlock()
		s.broadcastStatus(userIP, "has disconnected")
		log.Printf("User %s has been disconnected and cleaned up.\n", userIP)
	}()
	r := bufio.NewReader(conn)

	for {
		msg, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				fmt.Printf("User %s disconnected.\n", userIP)
				disconnectionMsg := fmt.Sprintf("User %s has disconnected.\n", userIP)
				s.broadcastMessage(disconnectionMsg)
			} else if strings.Contains(err.Error(), "use of closed network connection") {
				fmt.Printf("User %s has already been disconnected.\n", userIP)
			} else {
				fmt.Println("read error:", err)
			}
			return
		}

		if strings.HasPrefix(msg, "/") {
			s.handleCommand(conn, msg)
			continue
		}

		s.mu.Lock()
		s.userStatus[userIP].Typing = false
		s.userStatus[userIP].Offline = false
		s.userStatus[userIP].LastSeen = time.Now()
		s.mu.Unlock()

		s.msgch <- Message{
			from:    userIP,
			payload: []byte(msg),
		}
	}
}

func (s *Server) broadcastMessage(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for conn := range s.connections {
		conn.Write([]byte(message))
	}
}

func (s *Server) broadcastStatus(ip, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	message := fmt.Sprintf("User %s %s.\n", ip, status)
	for conn := range s.connections {
		conn.Write([]byte(message))
	}
}

func main() {
	// Initialize and start the server
	server := NewServer(":3001", "../cert.pem", "../key.pem")

	go func() {
		for msg := range server.msgch {
			fmt.Println("received message from connection:", msg.from, string(msg.payload))
			server.logMessage(msg)
		}
	}()

	go server.StartAdminPanel()
	log.Fatal(server.Start())
}
