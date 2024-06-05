package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"log"
	"os"
)

func main() {
	// Load client cert
	cert, err := tls.LoadX509KeyPair("../cert.pem", "../key.pem")
	if err != nil {
		log.Fatalf("failed to load client cert: %v", err)
	}

	// Set up a TLS config with the server's certificate
	tlsConfig := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true, // This should be set to false in production with a valid CA
	}

	// Dial the server with TLS
	conn, err := tls.Dial("tcp", "localhost:3001", tlsConfig)
	if err != nil {
		log.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	// Create a scanner to read user input from the terminal
	scanner := bufio.NewScanner(os.Stdin)

	fmt.Println("Connected to the server. Type your messages below:")

	// Start a goroutine to read messages from the server
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				log.Printf("failed to read response: %v", err)
				return
			}
			fmt.Printf("Received: %s", string(buf[:n]))
		}
	}()

	// Read user input and send messages to the server
	for scanner.Scan() {
		message := scanner.Text() + "\n"
		_, err = conn.Write([]byte(message))
		if err != nil {
			log.Fatalf("failed to send message: %v", err)
		}
	}
}
