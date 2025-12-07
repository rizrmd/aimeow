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
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	// Swagger docs
	_ "rizrmd/aimeow/docs"

	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

// @title Aimeow WhatsApp Bot API
// @version 1.0
// @description A REST API for managing multiple WhatsApp clients
// @BasePath /api/v1

// Helper function to get the base URL from the request
func getBaseURL(c *gin.Context) string {
	scheme := "http"
	if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := c.Request.Host
	if forwardedHost := c.GetHeader("X-Forwarded-Host"); forwardedHost != "" {
		host = forwardedHost
	}
	return fmt.Sprintf("%s://%s", scheme, host)
}

type WhatsAppClient struct {
	client        *whatsmeow.Client
	deviceStore   *store.Device
	isConnected   bool
	qrCode        string
	connectedAt   *time.Time
	messages      []string
	images        map[string]string // image_id -> file_path
	osName        string            // OS name to set after connection
	typingTimers  map[string]*time.Timer // chat_id -> typing timer
	typingActive  map[string]bool        // chat_id -> is currently typing
	mutex         sync.RWMutex
}

type ClientManager struct {
	clients       map[string]*WhatsAppClient
	container     *sqlstore.Container
	callbackURL   string
	configPath    string            // Path to configuration file
	clientIDMap   map[string]string // Maps WhatsApp device ID -> UUID
	clientMapPath string            // Path to client ID mapping file
	mutex         sync.RWMutex
}

// Config represents the persistent configuration
type Config struct {
	CallbackURL string `json:"callbackUrl"`
}

// ClientIDMapping represents the persistent mapping of WhatsApp IDs to UUIDs
type ClientIDMapping struct {
	Mappings map[string]string `json:"mappings"` // WhatsApp device ID -> UUID
}

var manager *ClientManager
var baseURL string // Base URL for generating file URLs in webhooks

func NewClientManager(container *sqlstore.Container, configPath string) *ClientManager {
	// Derive client map path from config path
	configDir := filepath.Dir(configPath)
	clientMapPath := filepath.Join(configDir, "client_mappings.json")

	cm := &ClientManager{
		clients:       make(map[string]*WhatsAppClient),
		container:     container,
		callbackURL:   "",
		configPath:    configPath,
		clientIDMap:   make(map[string]string),
		clientMapPath: clientMapPath,
	}
	// Load configuration from file
	if err := cm.loadConfig(); err != nil {
		fmt.Printf("Failed to load config (will use defaults): %v\n", err)
	}
	// Load client ID mappings
	if err := cm.loadClientMappings(); err != nil {
		fmt.Printf("Failed to load client mappings (will use defaults): %v\n", err)
	}
	return cm
}

// loadConfig loads configuration from JSON file
func (cm *ClientManager) loadConfig() error {
	data, err := os.ReadFile(cm.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Config file doesn't exist yet, that's okay
			return nil
		}
		return fmt.Errorf("failed to read config file: %w", err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("failed to parse config file: %w", err)
	}

	cm.mutex.Lock()
	cm.callbackURL = config.CallbackURL
	cm.mutex.Unlock()

	fmt.Printf("Configuration loaded: callbackURL=%s\n", config.CallbackURL)
	return nil
}

// saveConfig saves configuration to JSON file
func (cm *ClientManager) saveConfig() error {
	cm.mutex.RLock()
	config := Config{
		CallbackURL: cm.callbackURL,
	}
	cm.mutex.RUnlock()

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	// Write to temp file first, then rename for atomic operation
	tempPath := cm.configPath + ".tmp"
	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write temp config file: %w", err)
	}

	if err := os.Rename(tempPath, cm.configPath); err != nil {
		return fmt.Errorf("failed to rename temp config file: %w", err)
	}

	fmt.Printf("Configuration saved to %s\n", cm.configPath)
	return nil
}

// loadClientMappings loads client ID mappings from JSON file
func (cm *ClientManager) loadClientMappings() error {
	data, err := os.ReadFile(cm.clientMapPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Mapping file doesn't exist yet, that's okay
			return nil
		}
		return fmt.Errorf("failed to read client mappings file: %w", err)
	}

	var mapping ClientIDMapping
	if err := json.Unmarshal(data, &mapping); err != nil {
		return fmt.Errorf("failed to parse client mappings file: %w", err)
	}

	cm.mutex.Lock()
	cm.clientIDMap = mapping.Mappings
	if cm.clientIDMap == nil {
		cm.clientIDMap = make(map[string]string)
	}
	cm.mutex.Unlock()

	fmt.Printf("Client mappings loaded: %d mappings\n", len(mapping.Mappings))
	return nil
}

// saveClientMappings saves client ID mappings to JSON file
func (cm *ClientManager) saveClientMappings() error {
	cm.mutex.RLock()
	mapping := ClientIDMapping{
		Mappings: cm.clientIDMap,
	}
	cm.mutex.RUnlock()

	data, err := json.MarshalIndent(mapping, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal client mappings: %w", err)
	}

	// Write to temp file first, then rename for atomic operation
	tempPath := cm.clientMapPath + ".tmp"
	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write temp client mappings file: %w", err)
	}

	if err := os.Rename(tempPath, cm.clientMapPath); err != nil {
		return fmt.Errorf("failed to rename temp client mappings file: %w", err)
	}

	fmt.Printf("Client mappings saved to %s\n", cm.clientMapPath)
	return nil
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
		client:       client,
		deviceStore:  deviceStore,
		isConnected:  false,
		messages:     make([]string, 0),
		images:       make(map[string]string),
		osName:       osName, // Store OS name for later setting
		typingTimers: make(map[string]*time.Timer),
		typingActive: make(map[string]bool),
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

			// Mark message as read and start typing
			if !v.Info.IsFromMe {
				chatJID := v.Info.Chat
				go func() {
					// Mark as read
					err := client.client.MarkRead(context.Background(), []types.MessageID{v.Info.ID}, v.Info.Timestamp, chatJID, v.Info.Sender)
					if err != nil {
						fmt.Printf("Failed to mark message as read: %v\n", err)
					} else {
						fmt.Printf("Marked message as read from %s\n", chatJID.String())
					}

					// Start typing indicator
					cm.startTyping(client, chatJID)
				}()
			}

			// Send webhook callback if configured
			if cm.callbackURL != "" {
				go cm.sendWebhook(client, v)
			}

			// Download media if message contains media
			if v.Message.GetImageMessage() != nil || v.Message.GetVideoMessage() != nil || v.Message.GetAudioMessage() != nil || v.Message.GetDocumentMessage() != nil {
				fmt.Printf("Media message detected for client %s\n", client.deviceStore.ID.String())
				go cm.downloadImage(client, v)
			}
		case *events.Connected:
			client.isConnected = true
			now := time.Now()
			client.connectedAt = &now

			// Set OS name if provided and device has JID
			if client.osName != "" && client.deviceStore.ID != nil {
				store.DeviceProps.Os = &client.osName
			}

			// Save the mapping from WhatsApp device ID to our UUID
			// Find our UUID for this client by searching through manager.clients
			if client.deviceStore.ID != nil {
				whatsappID := client.deviceStore.ID.String()
				cm.mutex.Lock()
				// Find the UUID key for this client
				var ourUUID string
				for uuid, c := range cm.clients {
					if c == client {
						ourUUID = uuid
						break
					}
				}
				if ourUUID != "" {
					cm.clientIDMap[whatsappID] = ourUUID
					cm.mutex.Unlock()
					// Save mappings to disk
					if err := cm.saveClientMappings(); err != nil {
						fmt.Printf("Warning: Failed to save client mappings: %v\n", err)
					} else {
						fmt.Printf("Saved client mapping: %s -> %s\n", whatsappID, ourUUID)
					}
				} else {
					cm.mutex.Unlock()
				}
			}
		case *events.LoggedOut:
			client.isConnected = false
		case *events.QR:
			client.qrCode = v.Codes[0]
		}
	}
}

// startTyping starts the typing indicator for a chat and sets up a 1-minute timeout
func (cm *ClientManager) startTyping(client *WhatsAppClient, chatJID types.JID) {
	chatID := chatJID.String()

	client.mutex.Lock()
	defer client.mutex.Unlock()

	// If already typing for this chat, cancel the existing timer
	if timer, exists := client.typingTimers[chatID]; exists && timer != nil {
		timer.Stop()
	}

	// Send typing indicator
	err := client.client.SendChatPresence(context.Background(), chatJID, types.ChatPresenceComposing, types.ChatPresenceMediaText)
	if err != nil {
		fmt.Printf("Failed to send typing indicator: %v\n", err)
		return
	}

	client.typingActive[chatID] = true
	fmt.Printf("Started typing indicator for %s\n", chatID)

	// Set up timer to stop typing after 1 minute
	client.typingTimers[chatID] = time.AfterFunc(60*time.Second, func() {
		cm.stopTyping(client, chatJID)
	})
}

// stopTyping stops the typing indicator for a chat
func (cm *ClientManager) stopTyping(client *WhatsAppClient, chatJID types.JID) {
	chatID := chatJID.String()

	client.mutex.Lock()
	defer client.mutex.Unlock()

	// Check if we're actually typing for this chat
	if !client.typingActive[chatID] {
		return
	}

	// Cancel timer if it exists
	if timer, exists := client.typingTimers[chatID]; exists && timer != nil {
		timer.Stop()
		delete(client.typingTimers, chatID)
	}

	// Send stop typing indicator (paused)
	err := client.client.SendChatPresence(context.Background(), chatJID, types.ChatPresencePaused, types.ChatPresenceMediaText)
	if err != nil {
		fmt.Printf("Failed to stop typing indicator: %v\n", err)
	} else {
		fmt.Printf("Stopped typing indicator for %s\n", chatID)
	}

	delete(client.typingActive, chatID)
}

// Response structs
type ClientResponse struct {
	ID           string     `json:"id"`
	Phone        string     `json:"phone,omitempty"`
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

// Request and Response structs for sending messages
type SendMessageRequest struct {
	Phone    string `json:"phone" binding:"required"`
	Message  string `json:"message" binding:"required"`
}

type SendImageRequest struct {
	Phone     string `json:"phone" binding:"required"`
	ImageURL  string `json:"imageUrl" binding:"required,url"`
	Caption   string `json:"caption,omitempty"`
}

type SendMultipleImagesRequest struct {
	Phone     string `json:"phone" binding:"required"`
	Images    []ImageItem `json:"images" binding:"required,min=1"`
}

type ImageItem struct {
	ImageURL string `json:"imageUrl" binding:"required,url"`
	Caption  string `json:"caption,omitempty"`
}

type SendMessageResponse struct {
	Success   bool   `json:"success"`
	MessageID string `json:"messageId,omitempty"`
	Error     string `json:"error,omitempty"`
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

	qrURL := fmt.Sprintf("%s/qr?client_id=%s", getBaseURL(c), clientID)

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
		// Add phone number if device is connected
		if client.deviceStore != nil && client.deviceStore.ID != nil {
			resp.Phone = client.deviceStore.ID.User
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

	// Save configuration to persistent storage
	if err := manager.saveConfig(); err != nil {
		fmt.Printf("Warning: Failed to save config: %v\n", err)
		// Don't fail the request, just log the warning
	}

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

	// ClientID is now a UUID, no need to sanitize
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
	// Add phone number if device is connected
	if waClient.deviceStore != nil && waClient.deviceStore.ID != nil {
		resp.Phone = waClient.deviceStore.ID.User
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

// @Summary Send text message
// @Description Sends a text message to a WhatsApp number
// @Tags messages
// @Accept json
// @Produce json
// @Param id path string true "Client ID"
// @Param message body SendMessageRequest true "Message details"
// @Success 200 {object} SendMessageResponse
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /clients/{id}/send-message [post]
func sendMessage(c *gin.Context) {
	clientID := c.Param("id")

	waClient, err := manager.getClient(clientID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	if !waClient.isConnected {
		c.JSON(http.StatusBadRequest, gin.H{"error": "client is not connected"})
		return
	}

	var req SendMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Format phone number (remove @s.whatsapp.net if present, add if missing)
	targetJID := strings.TrimSuffix(req.Phone, "@s.whatsapp.net")
	if !strings.Contains(targetJID, "@") {
		targetJID += "@s.whatsapp.net"
	}

	// Parse JID
	targetJIDParsed, err := types.ParseJID(targetJID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid phone number format: %v", err)})
		return
	}

	// Stop typing indicator before sending message
	manager.stopTyping(waClient, targetJIDParsed)

	// Send message
	msg := &waE2E.Message{
		Conversation: &req.Message,
	}

	resp, err := waClient.client.SendMessage(context.Background(), targetJIDParsed, msg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, SendMessageResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to send message: %v", err),
		})
		return
	}

	c.JSON(http.StatusOK, SendMessageResponse{
		Success:   true,
		MessageID: resp.ID,
	})
}

// @Summary Send single image
// @Description Sends an image to a WhatsApp number
// @Tags messages
// @Accept json
// @Produce json
// @Param id path string true "Client ID"
// @Param image body SendImageRequest true "Image details"
// @Success 200 {object} SendMessageResponse
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /clients/{id}/send-image [post]
func sendImage(c *gin.Context) {
	clientID := c.Param("id")

	waClient, err := manager.getClient(clientID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	if !waClient.isConnected {
		c.JSON(http.StatusBadRequest, gin.H{"error": "client is not connected"})
		return
	}

	var req SendImageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Format phone number
	targetJID := strings.TrimSuffix(req.Phone, "@s.whatsapp.net")
	if !strings.Contains(targetJID, "@") {
		targetJID += "@s.whatsapp.net"
	}

	// Parse JID
	targetJIDParsed, err := types.ParseJID(targetJID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid phone number format: %v", err)})
		return
	}

	// Download image from URL
	resp, err := http.Get(req.ImageURL)
	if err != nil {
		c.JSON(http.StatusBadRequest, SendMessageResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to download image: %v", err),
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.JSON(http.StatusBadRequest, SendMessageResponse{
			Success: false,
			Error:   fmt.Sprintf("Image download failed with status: %d", resp.StatusCode),
		})
		return
	}

	// Read image data
	imageData, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, SendMessageResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to read image data: %v", err),
		})
		return
	}

	// Upload image to WhatsApp
	uploaded, err := waClient.client.Upload(context.Background(), imageData, whatsmeow.MediaImage)
	if err != nil {
		c.JSON(http.StatusInternalServerError, SendMessageResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to upload image to WhatsApp: %v", err),
		})
		return
	}

	// Create image message
	imageMsg := &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{
			URL:           proto.String(uploaded.URL),
			Mimetype:      proto.String(resp.Header.Get("Content-Type")),
			Caption:       proto.String(req.Caption),
			FileLength:    proto.Uint64(uint64(len(imageData))),
			FileSHA256:    uploaded.FileSHA256,
			FileEncSHA256: uploaded.FileEncSHA256,
			MediaKey:      uploaded.MediaKey,
			DirectPath:    proto.String(uploaded.DirectPath),
		},
	}

	// Stop typing indicator before sending image
	manager.stopTyping(waClient, targetJIDParsed)

	// Send the image message
	sendResp, err := waClient.client.SendMessage(context.Background(), targetJIDParsed, imageMsg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, SendMessageResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to send image: %v", err),
		})
		return
	}

	c.JSON(http.StatusOK, SendMessageResponse{
		Success:   true,
		MessageID: sendResp.ID,
	})
}

// @Summary Send multiple images
// @Description Sends multiple images to a WhatsApp number
// @Tags messages
// @Accept json
// @Produce json
// @Param id path string true "Client ID"
// @Param images body SendMultipleImagesRequest true "Multiple image details"
// @Success 200 {object} SendMessageResponse
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /clients/{id}/send-images [post]
func sendMultipleImages(c *gin.Context) {
	clientID := c.Param("id")

	waClient, err := manager.getClient(clientID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	if !waClient.isConnected {
		c.JSON(http.StatusBadRequest, gin.H{"error": "client is not connected"})
		return
	}

	var req SendMultipleImagesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Format phone number
	targetJID := strings.TrimSuffix(req.Phone, "@s.whatsapp.net")
	if !strings.Contains(targetJID, "@") {
		targetJID += "@s.whatsapp.net"
	}

	// Parse JID
	targetJIDParsed, err := types.ParseJID(targetJID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid phone number format: %v", err)})
		return
	}

	// Stop typing indicator before sending images
	manager.stopTyping(waClient, targetJIDParsed)

	var messageIDs []string
	var errors []string

	// Send each image
	for i, imageItem := range req.Images {
		// Download image from URL
		resp, err := http.Get(imageItem.ImageURL)
		if err != nil {
			errors = append(errors, fmt.Sprintf("Image %d: Failed to download - %v", i+1, err))
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			errors = append(errors, fmt.Sprintf("Image %d: Download failed with status %d", i+1, resp.StatusCode))
			continue
		}

		// Read image data
		imageData, err := io.ReadAll(resp.Body)
		if err != nil {
			errors = append(errors, fmt.Sprintf("Image %d: Failed to read data - %v", i+1, err))
			continue
		}

		// Upload image to WhatsApp
		uploaded, err := waClient.client.Upload(context.Background(), imageData, whatsmeow.MediaImage)
		if err != nil {
			errors = append(errors, fmt.Sprintf("Image %d: Upload failed - %v", i+1, err))
			continue
		}

		// Create image message
		imageMsg := &waE2E.Message{
			ImageMessage: &waE2E.ImageMessage{
				URL:           proto.String(uploaded.URL),
				Mimetype:      proto.String(resp.Header.Get("Content-Type")),
				Caption:       proto.String(imageItem.Caption),
				FileLength:    proto.Uint64(uint64(len(imageData))),
				FileSHA256:    uploaded.FileSHA256,
				FileEncSHA256: uploaded.FileEncSHA256,
				MediaKey:      uploaded.MediaKey,
				DirectPath:    proto.String(uploaded.DirectPath),
			},
		}

		// Send the image message
		sendResp, err := waClient.client.SendMessage(context.Background(), targetJIDParsed, imageMsg)
		if err != nil {
			errors = append(errors, fmt.Sprintf("Image %d: Send failed - %v", i+1, err))
			continue
		}

		messageIDs = append(messageIDs, sendResp.ID)
	}

	// Prepare response
	response := SendMessageResponse{
		Success: len(messageIDs) > 0,
	}

	if len(messageIDs) > 0 {
		response.MessageID = fmt.Sprintf("Sent %d images successfully. IDs: %s", len(messageIDs), strings.Join(messageIDs, ", "))
	}

	if len(errors) > 0 {
		if response.Success {
			response.Error = fmt.Sprintf("Partial success. Errors: %s", strings.Join(errors, "; "))
		} else {
			response.Error = fmt.Sprintf("All images failed. Errors: %s", strings.Join(errors, "; "))
		}
	}

	c.JSON(http.StatusOK, response)
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

	// Log the webhook payload for debugging
	fmt.Printf("[Aimeow Webhook] Payload: %s\n", string(jsonData))

	resp, err := http.Post(cm.callbackURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		fmt.Printf("Failed to send webhook: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		fmt.Printf("Webhook returned error status: %d\n", resp.StatusCode)
	} else {
		fmt.Printf("[Aimeow Webhook] Successfully sent to %s (status: %d)\n", cm.callbackURL, resp.StatusCode)
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

	// Get the UUID for this client
	whatsappID := client.deviceStore.ID.String()
	if whatsappID == "" {
		return
	}

	cm.mutex.RLock()
	clientID, exists := cm.clientIDMap[whatsappID]
	cm.mutex.RUnlock()

	// Fallback to WhatsApp ID if mapping doesn't exist
	if !exists {
		clientID = whatsappID
		fmt.Printf("Warning: No UUID mapping found for client %s\n", whatsappID)
	}

	// Create client directory using UUID (no need to sanitize since UUIDs don't have special chars)
	clientDir := filepath.Join("files", clientID)
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
	// Get the UUID for this client by looking up the WhatsApp ID in our mapping
	whatsappID := client.deviceStore.ID.String()
	cm.mutex.RLock()
	clientID, exists := cm.clientIDMap[whatsappID]
	cm.mutex.RUnlock()

	// Fallback to WhatsApp ID if mapping doesn't exist (shouldn't happen)
	if !exists {
		clientID = whatsappID
		fmt.Printf("Warning: No UUID mapping found for client %s\n", whatsappID)
	}

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

	// Extract sender - use Chat for individual chats, Sender for group chats
	var fromUser string
	if msg.Info.Chat.Server == types.DefaultUserServer {
		// Individual chat - use Chat.User which is more reliable
		fromUser = msg.Info.Chat.User
	} else if msg.Info.Chat.Server == types.GroupServer {
		// Group chat - use Sender.User to identify who sent the message
		fromUser = msg.Info.Sender.User
	} else {
		// Fallback to Sender.User for other cases
		fromUser = msg.Info.Sender.User
	}

	// Debug logging to help diagnose issues
	fmt.Printf("[Webhook Debug] Message from Chat=%s Sender=%s Using=%s\n",
		msg.Info.Chat.String(), msg.Info.Sender.String(), fromUser)

	messageData := map[string]interface{}{
		"id":        msg.Info.ID,
		"from":      fromUser,
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
			// Use clientID directly (it's already a UUID, no need to sanitize)
			messageData["fileUrl"] = fmt.Sprintf("%s/files/%s/%s", baseURL, clientID, msg.Info.ID)
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
			client:       client,
			deviceStore:  deviceStore,
			isConnected:  false,
			messages:     make([]string, 0),
			images:       make(map[string]string),
			osName:       "", // Empty for existing clients
			typingTimers: make(map[string]*time.Timer),
			typingActive: make(map[string]bool),
		}

		client.AddEventHandler(manager.eventHandler(waClient))

		// Use UUID from mapping if available, otherwise generate deterministic UUID from WhatsApp ID
		whatsappID := deviceStore.ID.String()
		manager.mutex.Lock()
		clientID, exists := manager.clientIDMap[whatsappID]
		if !exists {
			// Generate a new UUID for existing clients that don't have a mapping yet
			clientUUID := uuid.New()
			clientID = clientUUID.String()
			manager.clientIDMap[whatsappID] = clientID
			fmt.Printf("Generated new UUID for existing client %s: %s\n", whatsappID, clientID)
			// Save the new mapping
			manager.mutex.Unlock()
			if err := manager.saveClientMappings(); err != nil {
				fmt.Printf("Warning: Failed to save client mappings: %v\n", err)
			}
			manager.mutex.Lock()
		} else {
			fmt.Printf("Using existing UUID for client %s: %s\n", whatsappID, clientID)
		}
		manager.clients[clientID] = waClient
		manager.mutex.Unlock()

		// Auto-connect if device has existing session
		if client.Store.ID != nil {
			localClientID := clientID // Capture for closure
			go func() {
				err := client.Connect()
				if err != nil {
					fmt.Printf("Failed to reconnect client %s: %v\n", localClientID, err)
				}
			}()
		}
	}

	return nil
}

func main() {
	fmt.Println("Starting Aimeow WhatsApp API Server...")

	// Initialize base URL from environment variable
	baseURL = os.Getenv("BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:7030"
	}
	fmt.Printf("Base URL: %s\n", baseURL)

	// Initialize database
	dbLog := waLog.Stdout("Database", "DEBUG", true)
	ctx := context.Background()

	// Create files directory if it does not exist
	filesDir := "/app/files"
	if err := os.MkdirAll(filesDir, 0755); err != nil {
		fmt.Printf("Warning: Failed to create files directory: %v\n", err)
		// Try alternative directory
		filesDir = "/tmp/files"
		if err := os.MkdirAll(filesDir, 0755); err != nil {
			panic(fmt.Errorf("failed to create alternative files directory: %w", err))
		}
		fmt.Printf("Using alternative files directory: %s\n", filesDir)
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
		fmt.Printf("Database file creation test successful\n")
	}

	fmt.Printf("Initializing database container at: %s\n", dbPath)
	container, err := sqlstore.New(ctx, "sqlite3", fmt.Sprintf("file:%s?_foreign_keys=on", dbPath), dbLog)
	if err != nil {
		panic(fmt.Errorf("failed to initialize database container: %w", err))
	}
	fmt.Printf("Database container initialized successfully\n")

	// Initialize client manager with config path
	configPath := filepath.Join(filesDir, "config.json")
	fmt.Printf("Configuration file path: %s\n", configPath)
	manager = NewClientManager(container, configPath)

	// Load existing clients
	fmt.Printf("Loading existing clients...\n")
	err = loadExistingClients(container)
	if err != nil {
		fmt.Printf("Failed to load existing clients: %v\n", err)
	} else {
		fmt.Printf("Existing clients loaded successfully\n")
	}

	// Setup Gin router
	fmt.Printf("Setting up Gin router...\n")
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	// Configure CORS to allow all origins
	config := cors.DefaultConfig()
	config.AllowAllOrigins = true
	config.AllowMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}
	config.AllowHeaders = []string{"Origin", "Content-Length", "Content-Type", "Authorization"}
	r.Use(cors.New(config))
	fmt.Printf("CORS configured\n")

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

			// Send message endpoints
			clients.POST("/:id/send-message", sendMessage)
			clients.POST("/:id/send-image", sendImage)
			clients.POST("/:id/send-images", sendMultipleImages)
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
	fmt.Printf("Health check endpoint configured\n")

	// Fallback health check at root
	r.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Aimeow WhatsApp API is running"})
	})
	fmt.Printf("Root health check endpoint configured\n")

	// Swagger documentation
	r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))
	fmt.Printf("Swagger documentation configured\n")

	port := os.Getenv("PORT")
	if port == "" {
		port = "7030"
	}
	fmt.Printf("Aimeow WhatsApp API Server starting on :%s\n", port)
	fmt.Printf("Swagger UI: %s/swagger/index.html\n", baseURL)
	fmt.Printf("API Health: %s/health\n", baseURL)
	fmt.Printf("Files: %s/files\n", baseURL)

	fmt.Printf("Starting HTTP server on port %s...\n", port)
	r.Run(":" + port)
}
