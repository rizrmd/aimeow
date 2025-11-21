package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gin-contrib/cors"
	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	// Swagger docs
	_ "rizrmd/aimeow/docs"

	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

// @title Aimeow WhatsApp Bot API
// @version 1.0
// @description A REST API for managing multiple WhatsApp clients
// @host localhost:7030
// @BasePath /api/v1

type WhatsAppClient struct {
	client      *whatsmeow.Client
	deviceStore *store.Device
	isConnected bool
	qrCode      string
	connectedAt *time.Time
	messages    []string
	images      map[string]string // image_id -> file_path
	osName      string            // OS name to set after connection
	mutex       sync.RWMutex
}

type ClientManager struct {
	clients     map[string]*WhatsAppClient
	container   *sqlstore.Container
	callbackURL string
	mutex       sync.RWMutex
}

var manager *ClientManager

func NewClientManager(container *sqlstore.Container) *ClientManager {
	return &ClientManager{
		clients:     make(map[string]*WhatsAppClient),
		container:   container,
		callbackURL: "",
	}
}

func (cm *ClientManager) createClient(osName string) (*WhatsAppClient, string, error) {
	fmt.Printf("=== createClient called with osName: %s ===\n", osName)
	// Generate a UUID-based client ID that we'll use consistently
	clientUUID := uuid.New()
	clientID := clientUUID.String()
	fmt.Printf("Generated clientID: %s\n", clientID)

	// Create a device store with proper initialization but don't save to database yet
	fmt.Printf("Creating device store for client %s\n", clientID)
	deviceStore := cm.container.NewDevice()
	if deviceStore == nil {
		return nil, "", fmt.Errorf("failed to create new device: container returned nil")
	}
	fmt.Printf("Successfully created device store for client %s\n", clientID)

	clientLog := waLog.Stdout("Client", "DEBUG", true)
	fmt.Printf("About to create whatsmeow client for %s\n", clientID)
	client := whatsmeow.NewClient(deviceStore, clientLog)
	fmt.Printf("Successfully created whatsmeow client for %s\n", clientID)

	// Set OS name if provided before connection
	if osName != "" {
		fmt.Printf("Setting OS name '%s' for client before connection\n", osName)
		// The OS name should be set through the client's device store
		// This might be used during the initial registration/pairing process
	}

	waClient := &WhatsAppClient{
		client:      client,
		deviceStore: deviceStore,
		isConnected: false,
		messages:    make([]string, 0),
		images:      make(map[string]string),
		osName:      osName, // Store OS name for later setting
	}

	client.AddEventHandler(cm.eventHandler(waClient))

	// Store client with our generated UUID-based ID
	cm.mutex.Lock()
	manager.clients[clientID] = waClient
	cm.mutex.Unlock()

	return waClient, clientID, nil
}

func (cm *ClientManager) getClient(clientID string) (*WhatsAppClient, error) {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	client, exists := cm.clients[clientID]
	if !exists {
		return nil, fmt.Errorf("client not found")
	}
	return client, nil
}

func (cm *ClientManager) getAllClients() map[string]*WhatsAppClient {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	// Return a copy to avoid race conditions
	result := make(map[string]*WhatsAppClient)
	for id, client := range cm.clients {
		result[id] = client
	}
	return result
}

func (cm *ClientManager) eventHandler(client *WhatsAppClient) func(interface{}) {
	return func(evt interface{}) {
		client.mutex.Lock()
		defer client.mutex.Unlock()

		switch v := evt.(type) {
		case *events.Message:
			message := fmt.Sprintf("Message: %s", v.Message.GetConversation())
			client.messages = append(client.messages, message)
			if len(client.messages) > 100 { // Keep last 100 messages
				client.messages = client.messages[1:]
			}
			// Send webhook callback if configured
			if cm.callbackURL != "" {
				go cm.sendWebhook(client, v.Message)
			}

			// Download media if message contains media
			if v.Message.GetImageMessage() != nil || v.Message.GetVideoMessage() != nil || v.Message.GetAudioMessage() != nil || v.Message.GetDocumentMessage() != nil {
				fmt.Printf("Media message detected for client %s\n", client.deviceStore.ID.String())
				go cm.downloadImage(client, v.Message)
			}
		case *events.Connected:
			client.isConnected = true
			now := time.Now()
			client.connectedAt = &now

			// Set OS name if provided and device has JID
			if client.osName != "" && client.deviceStore.ID != nil {
				store.DeviceProps.Os = &client.osName
			}
		case *events.LoggedOut:
			client.isConnected = false
		case *events.QR:
			client.qrCode = v.Codes[0]
		}
	}
}

// Response structs
type ClientResponse struct {
	ID           string     `json:"id"`
	IsConnected  bool       `json:"isConnected"`
	QRCode       string     `json:"qrCode,omitempty"`
	ConnectedAt  *time.Time `json:"connectedAt,omitempty"`
	MessageCount int        `json:"messageCount"`
}

type CreateClientResponse struct {
	ID    string `json:"id"`
	QRURL string `json:"qrUrl"`
}

type CreateClientRequest struct {
	OSName string `json:"osName,omitempty"`
}

type ConfigRequest struct {
	CallbackURL string `json:"callbackUrl" binding:"required,url"`
}

type ConfigResponse struct {
	CallbackURL string `json:"callbackUrl"`
}

type MessageResponse struct {
	Messages []string `json:"messages"`
}

// @Summary Create a new WhatsApp client
// @Description Creates a new WhatsApp client and returns QR code for pairing
// @Tags clients
// @Accept json
// @Produce json
// @Param config body CreateClientRequest false "Configuration object"
// @Success 200 {object} CreateClientResponse
// @Failure 500 {object} map[string]string
// @Router /clients/new [post]
func createClient(c *gin.Context) {
	fmt.Printf("=== createClient API endpoint called ===\n")
	var req CreateClientRequest

	// Parse request body (empty body is also allowed)
	if err := c.ShouldBindJSON(&req); err != nil {
		// If binding fails due to empty body, use empty struct
		req = CreateClientRequest{}
	}
	fmt.Printf("Request parsed. OSName: '%s'\n", req.OSName)

	fmt.Printf("About to call manager.createClient...\n")
	waClient, clientID, err := manager.createClient(req.OSName)
	fmt.Printf("manager.createClient returned. err: %v\n", err)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Start connection process
	go func() {
		qrChan, err := waClient.client.GetQRChannel(context.Background())
		if err != nil {
			fmt.Printf("Failed to get QR channel: %v\n", err)
			return
		}

		err = waClient.client.Connect()
		if err != nil {
			fmt.Printf("Failed to connect client: %v\n", err)
			return
		}

		for evt := range qrChan {
			if evt.Event == "code" {
				waClient.mutex.Lock()
				waClient.qrCode = evt.Code
				waClient.mutex.Unlock()
				fmt.Printf("QR code generated for client %s\n", clientID)
			}
		}
	}()

	// Start connection process
	go func() {
		qrChan, err := waClient.client.GetQRChannel(context.Background())
		if err != nil {
			fmt.Printf("Failed to get QR channel: %v\n", err)
			return
		}

		err = waClient.client.Connect()
		if err != nil {
			fmt.Printf("Failed to connect client: %v\n", err)
			return
		}

		for evt := range qrChan {
			if evt.Event == "code" {
				waClient.mutex.Lock()
				waClient.qrCode = evt.Code
				waClient.mutex.Unlock()
				clientID := ""
				if waClient.deviceStore.ID != nil {
					clientID = waClient.deviceStore.ID.String()
				}
				fmt.Printf("QR code generated for client %s\n", clientID)
			}
		}
	}()

	qrURL := fmt.Sprintf("http://localhost:7030/qr?client_id=%s", clientID)

	c.JSON(http.StatusOK, CreateClientResponse{
		ID:    clientID,
		QRURL: qrURL,
	})
}

// @Summary Get all clients
// @Description Returns list of all WhatsApp clients
// @Tags clients
// @Accept json
// @Produce json
// @Success 200 {array} ClientResponse
// @Router /clients [get]
func getAllClients(c *gin.Context) {
	clients := manager.getAllClients()

	response := make([]ClientResponse, 0)
	for id, client := range clients {
		client.mutex.RLock()
		resp := ClientResponse{
			ID:           id,
			IsConnected:  client.isConnected,
			QRCode:       client.qrCode,
			ConnectedAt:  client.connectedAt,
			MessageCount: len(client.messages),
		}
		if resp.QRCode == "" {
			resp.QRCode = "not_available"
		}
		response = append(response, resp)
		client.mutex.RUnlock()
	}

	c.JSON(http.StatusOK, response)
}

// @Summary Set webhook callback URL
// @Description Sets the callback URL for receiving message webhooks
// @Tags config
// @Accept json
// @Produce json
// @Param config body ConfigRequest true "Configuration object"
// @Success 200 {object} ConfigResponse
// @Failure 400 {object} map[string]string
// @Router /config [post]
func setConfig(c *gin.Context) {
	var req ConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	manager.mutex.Lock()
	manager.callbackURL = req.CallbackURL
	manager.mutex.Unlock()

	c.JSON(http.StatusOK, ConfigResponse{
		CallbackURL: req.CallbackURL,
	})
}

// @Summary Get current configuration
// @Description Gets the current callback URL configuration
// @Tags config
// @Accept json
// @Produce json
// @Success 200 {object} ConfigResponse
// @Router /config [get]
func getConfig(c *gin.Context) {
	manager.mutex.RLock()
	callbackURL := manager.callbackURL
	manager.mutex.RUnlock()

	c.JSON(http.StatusOK, ConfigResponse{
		CallbackURL: callbackURL,
	})
}

// @Summary Get client file
// @Description Gets a media file for a specific client
// @Tags files
// @Accept json
// @Produce application/octet-stream
// @Param client_id path string true "Client ID"
// @Param file_id path string true "File ID"
// @Success 200 {file} file "Media file"
// @Failure 404 {object} map[string]string
// @Router /files/{client_id}/{file_id} [get]
func getClientFile(c *gin.Context) {
	clientID := c.Param("client_id")
	fileID := c.Param("file_id")

	// Sanitize client ID (convert back from filename format)
	clientID = strings.ReplaceAll(clientID, "_", "@")

	waClient, err := manager.getClient(clientID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "client not found"})
		return
	}

	waClient.mutex.RLock()
	filePath, exists := waClient.images[fileID]
	waClient.mutex.RUnlock()

	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		return
	}

	// Serve the media file
	c.File(filePath)
}

// @Summary Get client by ID
// @Description Returns details of a specific WhatsApp client
// @Tags clients
// @Accept json
// @Produce json
// @Param id path string true "Client ID"
// @Success 200 {object} ClientResponse
// @Failure 404 {object} map[string]string
// @Router /clients/{id} [get]
func getClient(c *gin.Context) {
	clientID := c.Param("id")

	waClient, err := manager.getClient(clientID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	waClient.mutex.RLock()
	resp := ClientResponse{
		ID:           clientID,
		IsConnected:  waClient.isConnected,
		QRCode:       waClient.qrCode,
		ConnectedAt:  waClient.connectedAt,
		MessageCount: len(waClient.messages),
	}
	if resp.QRCode == "" {
		resp.QRCode = "not_available"
	}
	waClient.mutex.RUnlock()

	c.JSON(http.StatusOK, resp)
}

// @Summary Get QR code for client (terminal format)
// @Description Returns QR code in terminal format for client pairing
// @Tags clients
// @Accept json
// @Produce plain
// @Param id path string true "Client ID"
// @Success 200 {string} string "QR code in terminal format"
// @Failure 404 {object} map[string]string
// @Router /clients/{id}/qr [get]
func getQRCode(c *gin.Context) {
	clientID := c.Param("id")

	waClient, err := manager.getClient(clientID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	waClient.mutex.RLock()
	qrCode := waClient.qrCode
	waClient.mutex.RUnlock()

	if qrCode == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "QR code not available"})
		return
	}

	// Generate terminal QR code
	c.Header("Content-Type", "text/plain; charset=utf-8")
	qrterminal.Generate(qrCode, qrterminal.L, c.Writer)
}

// @Summary Get QR code for client (HTML format)
// @Description Returns QR code in HTML format for easy display in browser
// @Tags clients
// @Accept json
// @Produce html
// @Param client_id query string true "Client ID"
// @Success 200 {string} string "HTML page with QR code"
// @Failure 404 {object} map[string]string
// @Router /qr [get]
func getQRCodeHTML(c *gin.Context) {
	clientID := c.Query("client_id")
	if clientID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "client_id parameter is required"})
		return
	}

	waClient, err := manager.getClient(clientID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	waClient.mutex.RLock()
	qrCode := waClient.qrCode
	isConnected := waClient.isConnected
	waClient.mutex.RUnlock()

	htmlContent := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
    <title>WhatsApp QR Code - Client %s</title>
    <style>
        body { font-family: Arial, sans-serif; text-align: center; padding: 20px; }
        .container { max-width: 600px; margin: 0 auto; }
        .qr-code { 
            margin: 20px auto; 
            padding: 20px; 
            border: 2px solid #ddd; 
            border-radius: 10px; 
            background: white;
            white-space: pre;
            font-family: monospace;
            line-height: 1;
            font-size: 2px;
        }
        .info { 
            margin: 20px 0; 
            padding: 15px; 
            background: #f0f8ff; 
            border-radius: 5px; 
        }
        .refresh { 
            margin: 10px 0; 
        }
        .refresh button {
            padding: 10px 20px;
            background: #25d366;
            color: white;
            border: none;
            border-radius: 5px;
            cursor: pointer;
            font-size: 16px;
        }
        .refresh button:hover {
            background: #128c7e;
        }
    </style>
    <script>
        function refreshQR() {
            setTimeout(() => {
                location.reload();
            }, 5000);
        }
        
        // Auto-refresh every 10 seconds if not connected
        setInterval(() => {
            fetch('/api/v1/clients/%s')
                .then(response => response.json())
                .then(data => {
                    if (data.isConnected) {
                        location.reload();
                    }
                })
                .catch(() => {});
        }, 10000);
    </script>
</head>
<body>
    <div class="container">
        <h1>WhatsApp QR Code</h1>
        <div class="info">
            <strong>Client ID:</strong> %s<br>
            <strong>Status:</strong> %s
        </div>`, strings.ReplaceAll(clientID, "<", "&lt;"), strings.ReplaceAll(clientID, "<", "&lt;"), strings.ReplaceAll(clientID, "<", "&lt;"), map[bool]string{true: "Connected", false: "Waiting for QR scan"}[isConnected])

	if isConnected {
		htmlContent += `
        <div class="info" style="background: #d4edda; color: #155724;">
            <h2>✅ Connected Successfully!</h2>
            <p>Your WhatsApp client is now connected and ready to use.</p>
        </div>`
	} else if qrCode == "" {
		htmlContent += `
        <div class="info" style="background: #fff3cd; color: #856404;">
            <h2>⏳ Waiting for QR Code...</h2>
            <p>QR code is being generated. Please wait.</p>
        </div>
        <div class="refresh">
            <button onclick="refreshQR()">Refresh</button>
        </div>`
	} else {
		// Generate QR code as ASCII art using qrterminal
		htmlContent += `
        <div class="info">
            <h2>📱 Scan this QR code with WhatsApp</h2>
            <p>Open WhatsApp on your phone → Linked Devices → Link a device</p>
        </div>
        <div class="refresh">
            <button onclick="refreshQR()">Refresh QR Code</button>
        </div>
        <div class="qr-code">`

		// Capture qrterminal output
		var qrBuilder strings.Builder
		qrterminal.GenerateWithConfig(qrCode, qrterminal.Config{
			Level:     qrterminal.L,
			Writer:    &qrBuilder,
			BlackChar: "██",
			WhiteChar: "  ",
		})

		// Convert to HTML-safe format
		qrHTML := strings.ReplaceAll(qrBuilder.String(), " ", "&nbsp;")
		qrHTML = strings.ReplaceAll(qrHTML, "\n", "<br>")
		htmlContent += qrHTML

		htmlContent += `</div>`
	}

	htmlContent += `
        <div class="info" style="margin-top: 30px; font-size: 14px;">
            <p>This page will automatically refresh when the connection status changes.</p>
            <p><a href="/api/v1/clients/%s">View API details</a></p>
        </div>
    </div>
</body>
</html>`

	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, htmlContent)
}

// @Summary Get messages for client
// @Description Returns recent messages for a specific WhatsApp client
// @Tags messages
// @Accept json
// @Produce json
// @Param id path string true "Client ID"
// @Param limit query int false "Limit number of messages" default(50)
// @Success 200 {object} MessageResponse
// @Failure 404 {object} map[string]string
// @Router /clients/{id}/messages [get]
func getMessages(c *gin.Context) {
	clientID := c.Param("id")

	waClient, err := manager.getClient(clientID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	limit := 50
	if l := c.Query("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	waClient.mutex.RLock()
	messages := waClient.messages
	if len(messages) > limit {
		messages = messages[len(messages)-limit:]
	}
	response := MessageResponse{Messages: make([]string, len(messages))}
	copy(response.Messages, messages)
	waClient.mutex.RUnlock()

	c.JSON(http.StatusOK, response)
}

// @Summary Disconnect and delete client
// @Description Disconnects and removes a WhatsApp client
// @Tags clients
// @Accept json
// @Produce json
// @Param id path string true "Client ID"
// @Success 200 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Router /clients/{id} [delete]
func deleteClient(c *gin.Context) {
	clientID := c.Param("id")

	waClient, err := manager.getClient(clientID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	// Disconnect if connected
	if waClient.client.IsConnected() {
		waClient.client.Disconnect()
	}

	// Remove from manager
	manager.mutex.Lock()
	delete(manager.clients, clientID)
	manager.mutex.Unlock()

	c.JSON(http.StatusOK, gin.H{"message": "client deleted successfully"})
}

func (cm *ClientManager) sendWebhook(client *WhatsAppClient, message interface{}) {
	if cm.callbackURL == "" {
		return
	}

	// Extract message data from the message interface
	webhookData := cm.extractMessageData(client, message)

	jsonData, err := json.Marshal(webhookData)
	if err != nil {
		fmt.Printf("Failed to marshal webhook data: %v\n", err)
		return
	}

	resp, err := http.Post(cm.callbackURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		fmt.Printf("Failed to send webhook: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		fmt.Printf("Webhook returned error status: %d\n", resp.StatusCode)
	}
}

func (cm *ClientManager) downloadImage(client *WhatsAppClient, message interface{}) {
	// Type assert to get the actual message struct
	msg, ok := message.(*events.Message)
	if !ok {
		return
	}

	// Check if message contains media
	if msg.Message.GetImageMessage() == nil && msg.Message.GetVideoMessage() == nil && msg.Message.GetAudioMessage() == nil && msg.Message.GetDocumentMessage() == nil {
		fmt.Printf("No media in message, skipping download\n")
		return
	}

	fmt.Printf("Media detected in message, proceeding with download\n")

	// Create client directory if it doesn't exist
	clientID := client.deviceStore.ID.String()
	if clientID == "" {
		return
	}

	clientDir := filepath.Join("files", strings.ReplaceAll(clientID, "@", "_"))
	err := os.MkdirAll(clientDir, 0755)
	if err != nil {
		fmt.Printf("Failed to create client directory: %v\n", err)
		return
	}

	var fileExtension string
	var downloadURL string
	var mediaType string

	// Handle different media types
	switch {
	case msg.Message.GetImageMessage() != nil:
		// Handle image
		imageMsg := msg.Message.GetImageMessage()
		if imageMsg.GetDirectPath() == "" {
			return
		}
		mediaType = "image"
		fileExtension = ".jpg" // Default for images
		if imageMsg.GetMimetype() == "image/png" {
			fileExtension = ".png"
		} else if imageMsg.GetMimetype() == "image/webp" {
			fileExtension = ".webp"
		} else if imageMsg.GetMimetype() == "image/gif" {
			fileExtension = ".gif"
		}
		downloadURL = fmt.Sprintf("https://mmg.whatsapp.net/d/%s", imageMsg.GetDirectPath())

	case msg.Message.GetVideoMessage() != nil:
		// Handle video
		videoMsg := msg.Message.GetVideoMessage()
		if videoMsg.GetDirectPath() == "" {
			return
		}
		mediaType = "video"
		fileExtension = ".mp4" // Default for videos
		if videoMsg.GetMimetype() == "video/3gpp" {
			fileExtension = ".3gp"
		} else if videoMsg.GetMimetype() == "video/webm" {
			fileExtension = ".webm"
		}
		downloadURL = fmt.Sprintf("https://mmg.whatsapp.net/d/%s", videoMsg.GetDirectPath())

	case msg.Message.GetAudioMessage() != nil:
		// Handle audio
		audioMsg := msg.Message.GetAudioMessage()
		if audioMsg.GetDirectPath() == "" {
			return
		}
		mediaType = "audio"
		fileExtension = ".ogg" // Default for WhatsApp audio
		if audioMsg.GetMimetype() == "audio/mpeg" {
			fileExtension = ".mp3"
		} else if audioMsg.GetMimetype() == "audio/mp4" {
			fileExtension = ".m4a"
		}
		downloadURL = fmt.Sprintf("https://mmg.whatsapp.net/d/%s", audioMsg.GetDirectPath())

	case msg.Message.GetDocumentMessage() != nil:
		// Handle document
		docMsg := msg.Message.GetDocumentMessage()
		if docMsg.GetDirectPath() == "" {
			return
		}
		mediaType = "document"
		// Extract extension from filename
		fileName := docMsg.GetFileName()
		if ext := filepath.Ext(fileName); ext != "" {
			fileExtension = ext
		} else {
			fileExtension = ".bin" // Default for unknown documents
		}
		downloadURL = fmt.Sprintf("https://mmg.whatsapp.net/d/%s", docMsg.GetDirectPath())

	default:
		return // No media to download
	}

	mediaID := msg.Info.ID
	mediaPath := filepath.Join(clientDir, mediaID+fileExtension)

	// Download the image
	resp, err := http.Get(downloadURL)
	if err != nil {
		fmt.Printf("Failed to download %s for client %s: %v\n", mediaType, clientID, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("%s download failed with status %d for client %s\n", mediaType, resp.StatusCode, clientID)
		return
	}

	// Save media to file
	file, err := os.Create(mediaPath)
	if err != nil {
		fmt.Printf("Failed to create %s file for client %s: %v\n", mediaType, clientID, err)
		return
	}
	defer file.Close()

	_, err = io.Copy(file, resp.Body)
	if err != nil {
		fmt.Printf("Failed to save %s file for client %s: %v\n", mediaType, clientID, err)
		return
	}

	// Store media path in client
	client.mutex.Lock()
	client.images[mediaID] = mediaPath
	client.mutex.Unlock()

	fmt.Printf("%s downloaded for client %s: %s -> %s\n", strings.Title(mediaType), clientID, mediaID, mediaPath)
}

func (cm *ClientManager) extractMessageData(client *WhatsAppClient, message interface{}) map[string]interface{} {
	clientID := client.deviceStore.ID.String()

	// Type assert to get actual message struct
	msg, ok := message.(*events.Message)
	if !ok {
		return map[string]interface{}{
			"clientId": clientID,
			"message": map[string]interface{}{
				"type": "unknown",
			},
			"timestamp": time.Now().Unix(),
		}
	}

	messageData := map[string]interface{}{
		"id":        msg.Info.ID,
		"from":      msg.Info.Sender.User,
		"timestamp": msg.Info.Timestamp,
		"pushName":  msg.Info.PushName,
	}

	// Determine message type and extract content
	switch {
	case msg.Message.GetConversation() != "":
		// Text message
		messageData["type"] = "text"
		messageData["text"] = msg.Message.GetConversation()

	case msg.Message.GetImageMessage() != nil:
		// Image message
		imgMsg := msg.Message.GetImageMessage()
		messageData["type"] = "image"
		messageData["caption"] = imgMsg.GetCaption()
		messageData["mimeType"] = imgMsg.GetMimetype()
		messageData["width"] = imgMsg.GetWidth()
		messageData["height"] = imgMsg.GetHeight()
		if imgMsg.GetFileLength() > 0 {
			messageData["fileSize"] = imgMsg.GetFileLength()
		}

	default:
		// Other message types
		messageData["type"] = "other"
	}

	// Add file access URL if media file was downloaded
	if messageData["type"] != "text" {
		client.mutex.RLock()
		if _, exists := client.images[msg.Info.ID]; exists {
			sanitizedClientID := strings.ReplaceAll(clientID, "@", "_")
			messageData["fileUrl"] = fmt.Sprintf("http://localhost:7030/files/%s/%s", sanitizedClientID, msg.Info.ID)
		}
		client.mutex.RUnlock()
	}

	return map[string]interface{}{
		"clientId":  clientID,
		"message":   messageData,
		"timestamp": time.Now().Unix(),
	}
}

func loadExistingClients(container *sqlstore.Container) error {
	ctx := context.Background()
	devices, err := container.GetAllDevices(ctx)
	if err != nil {
		return fmt.Errorf("failed to get existing devices: %w", err)
	}

	for _, deviceStore := range devices {
		clientLog := waLog.Stdout("Client", "DEBUG", true)
		client := whatsmeow.NewClient(deviceStore, clientLog)

		waClient := &WhatsAppClient{
			client:      client,
			deviceStore: deviceStore,
			isConnected: false,
			messages:    make([]string, 0),
			images:      make(map[string]string),
			osName:      "", // Empty for existing clients
		}

		client.AddEventHandler(manager.eventHandler(waClient))

		clientID := deviceStore.ID.String()
		manager.clients[clientID] = waClient

		// Auto-connect if device has existing session
		if client.Store.ID != nil {
			go func() {
				err := client.Connect()
				if err != nil {
					fmt.Printf("Failed to reconnect client %s: %v\n", clientID, err)
				}
			}()
		}
	}

	return nil
}

func main() {
	// Initialize database
	dbLog := waLog.Stdout("Database", "DEBUG", true)
	ctx := context.Background()

	//create files directory if it does not exists
	filesDir := "/app/files"
	if err := os.MkdirAll(filesDir, 0755); err != nil {
		panic(fmt.Errorf("failed to create files directory: %w", err))
	}

	// Create database path
	dbPath := filepath.Join(filesDir, "aimeow.db")
	fmt.Printf("Database path: %s\n", dbPath)

	// Try to create the database file to check permissions
	dbFile, err := os.Create(dbPath)
	if err != nil {
		fmt.Printf("Warning: Cannot create database file: %v\n", err)
		// Try alternative location in /tmp
		dbPath = "/tmp/aimeow.db"
		fmt.Printf("Using fallback database path: %s\n", dbPath)
	} else {
		dbFile.Close()
		os.Remove(dbPath) // Remove empty file, let sqlstore create it properly
	}

	container, err := sqlstore.New(ctx, "sqlite3", fmt.Sprintf("file:%s?_foreign_keys=on", dbPath), dbLog)
	if err != nil {
		panic(fmt.Errorf("failed to initialize database container: %w", err))
	}

	// Initialize client manager
	manager = NewClientManager(container)

	// Load existing clients
	err = loadExistingClients(container)
	if err != nil {
		fmt.Printf("Failed to load existing clients: %v\n", err)
	}

	// Setup Gin router
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	// Configure CORS to allow all origins
	config := cors.DefaultConfig()
	config.AllowAllOrigins = true
	config.AllowMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}
	config.AllowHeaders = []string{"Origin", "Content-Length", "Content-Type", "Authorization"}
	r.Use(cors.New(config))

	// API routes
	v1 := r.Group("/api/v1")
	{
		clients := v1.Group("/clients")
		{
			clients.POST("/new", createClient)
			clients.GET("", getAllClients)
			clients.GET("/:id", getClient)
			clients.GET("/:id/qr", getQRCode)
			clients.GET("/:id/messages", getMessages)
			clients.DELETE("/:id", deleteClient)
		}

		// Config endpoints
		v1.POST("/config", setConfig)
		v1.GET("/config", getConfig)

		// QR code HTML endpoint
		r.GET("/qr", getQRCodeHTML)

		// Serve client files
		r.GET("/files/:client_id/:file_id", getClientFile)
	}

	// Health check
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Swagger documentation
	r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	fmt.Println("Aimeow WhatsApp API Server starting on :7030")
	fmt.Println("Swagger UI: http://localhost:7030/swagger/index.html")
	fmt.Println("API Health: http://localhost:7030/health")
	fmt.Println("Files: http://localhost:7030/files")

	r.Run(":7030")
}
