package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/ua"
	"github.com/pelletier/go-toml"
)

type NodeDef struct {
	ID       string
	Name     string
	DataType string
	Children []*NodeDef
	Checked  bool
}

var (
	rootNodes     []*NodeDef
	selectedNodes []*NodeDef
	mainWindow    fyne.Window
)

func main() {
	// Create application
	a := app.New()
	mainWindow = a.NewWindow("Telegraf OPC UA Configurator")
	mainWindow.Resize(fyne.NewSize(600, 600))

	// UI elements
	urlEntry := widget.NewEntry()
	urlEntry.SetPlaceHolder("opc.tcp://localhost:4840")

	statusLabel := widget.NewLabel("Ready to connect")
	connectBtn := widget.NewButton("Connect to OPC UA Server", func() {
		statusLabel.SetText("Connecting...")
		err := discoverNodes(urlEntry.Text)
		if err != nil {
			dialog.ShowError(err, mainWindow)
			statusLabel.SetText("Connection failed")
			return
		}
		statusLabel.SetText("Connected successfully")
		mainWindow.SetContent(createMainUI(urlEntry.Text))
	})

	// Initial connection screen
	initialScreen := container.NewVBox(
		widget.NewLabel("OPC UA Server Configuration"),
		urlEntry,
		connectBtn,
		statusLabel,
	)

	mainWindow.SetContent(initialScreen)
	mainWindow.ShowAndRun()
}

func discoverNodes(endpoint string) error {
	log.Printf("Starting node discovery for endpoint: %s", endpoint)

	// Parse the endpoint URL to get host and port
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("invalid endpoint URL: %w", err)
	}

	// Extract host and port
	host := u.Hostname()
	port := u.Port()

	// Try to resolve the hostname
	log.Printf("Resolving hostname: %s", host)
	ips, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("unable to resolve host %s: %w", host, err)
	}
	log.Printf("Resolved IPs: %v", ips)

	// Try to connect to the port
	log.Printf("Testing TCP connection to %s:%s", host, port)
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%s", host, port), 5*time.Second)
	if err != nil {
		return fmt.Errorf("cannot connect to %s:%s: %w", host, port, err)
	}
	conn.Close()
	log.Printf("TCP connection successful")

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // Increased timeout
	defer cancel()

	// Try different security configurations
	securityConfigs := [][]opcua.Option{
		{
			opcua.SecurityMode(ua.MessageSecurityModeNone),
			opcua.SecurityPolicy("None"),
			opcua.AutoReconnect(true),
		},
		{
			opcua.SecurityMode(ua.MessageSecurityModeSign),
			opcua.SecurityPolicy("Basic256"),
			opcua.AutoReconnect(true),
		},
		{
			opcua.SecurityMode(ua.MessageSecurityModeSignAndEncrypt),
			opcua.SecurityPolicy("Basic256Sha256"),
			opcua.AutoReconnect(true),
		},
	}

	var lastErr error
	for _, opts := range securityConfigs {
		log.Printf("Trying connection with security mode: %v", opts[0])

		c, err := opcua.NewClient(endpoint, opts...)
		if err != nil {
			lastErr = err
			log.Printf("Client creation failed: %v", err)
			continue
		}

		log.Printf("Attempting to connect...")
		if err := c.Connect(ctx); err != nil {
			lastErr = err
			log.Printf("Connection failed: %v", err)
			continue
		}

		log.Printf("Connected successfully")

		defer func() {
			log.Printf("Closing connection...")
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer closeCancel()
			if err := c.Close(closeCtx); err != nil {
				log.Printf("Error closing connection: %v", err)
			}
		}()

		// Verify connection
		log.Printf("Verifying connection...")
		req := &ua.ReadRequest{
			NodesToRead: []*ua.ReadValueID{
				{NodeID: ua.NewNumericNodeID(0, 2256)}, // Server time
			},
		}

		_, err = c.Read(ctx, req)
		if err != nil {
			lastErr = err
			log.Printf("Connection verification failed: %v", err)
			continue
		}

		// If we get here, connection is successful
		log.Printf("Connection verified successfully")

		// Browse root node
		root := &NodeDef{
			ID:   "i=85", // Objects folder
			Name: "Objects",
		}
		rootNodes = []*NodeDef{root}
		if err := browseNode(c, root); err != nil {
			return fmt.Errorf("browsing nodes failed: %w", err)
		}

		if len(root.Children) == 0 {
			return fmt.Errorf("no nodes found in the server")
		}

		rootNodes = []*NodeDef{root}
		return nil
	}

	// If we get here, all connection attempts failed
	return fmt.Errorf("all connection attempts failed, last error: %w", lastErr)
}

func browseNode(c *opcua.Client, parent *NodeDef) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := &ua.BrowseRequest{
		NodesToBrowse: []*ua.BrowseDescription{{
			NodeID:          ua.MustParseNodeID(parent.ID),
			BrowseDirection: ua.BrowseDirectionForward,
			ReferenceTypeID: ua.NewNumericNodeID(0, 0), // All references
			IncludeSubtypes: true,
			ResultMask:      uint32(ua.BrowseResultMaskAll),
		}},
	}

	res, err := c.Browse(ctx, req)
	if err != nil {
		return fmt.Errorf("browse failed for %q: %w", parent.ID, err)
	}

	for _, result := range res.Results {
		if result.StatusCode != ua.StatusOK {
			continue
		}

		for _, ref := range result.References {
			if ref == nil || ref.NodeID == nil {
				continue
			}

			// Extract the actual NodeID from ExpandedNodeID
			nodeID := ref.NodeID.NodeID
			if nodeID == nil {
				continue
			}

			// Skip historical nodes (ns=1) and self-references
			if nodeID.Namespace() == 1 || nodeID.String() == parent.ID {
				continue
			}

			// Get node name (fallback to NodeID if no display name)
			name := ref.DisplayName.Text
			if name == "" {
				name = nodeID.String()
			}

			node := &NodeDef{
				ID:   nodeID.String(),
				Name: name,
			}

			if !nodeExists(parent.Children, node.ID) {
				parent.Children = append(parent.Children, node)

				// Browse child nodes if it's an object or variable
				if ref.NodeClass == ua.NodeClassObject || ref.NodeClass == ua.NodeClassVariable {
					if err := browseNode(c, node); err != nil {
						log.Printf("failed to browse %q: %v", node.ID, err)
					}
				}
			}
		}
	}
	return nil
}

func nodeExists(children []*NodeDef, id string) bool {
	for _, child := range children {
		if child.ID == id {
			return true
		}
	}
	return false
}

func parseNodeID(id string) (namespace, idType, identifier string, err error) {
	parts := strings.SplitN(id, ";", 2)
	if len(parts) != 2 {
		return "", "", "", fmt.Errorf("invalid node ID format")
	}

	// Parse namespace part
	nsPart := strings.TrimPrefix(parts[0], "ns=")
	nsParts := strings.SplitN(nsPart, "=", 2)
	if len(nsParts) != 1 {
		return "", "", "", fmt.Errorf("invalid namespace format")
	}
	namespace = nsParts[0]

	// Parse identifier part
	idPart := parts[1]
	idParts := strings.SplitN(idPart, "=", 2)
	if len(idParts) != 2 {
		return "", "", "", fmt.Errorf("invalid identifier format")
	}
	idType = idParts[0]
	identifier = idParts[1]

	return namespace, idType, identifier, nil
}

func createMainUI(endpoint string) fyne.CanvasObject {
	// Create tree view
	tree := widget.NewTree(
		func(id widget.TreeNodeID) []widget.TreeNodeID {
			// Handle root nodes when id is empty
			if id == "" {
				var roots []string
				for _, root := range rootNodes {
					roots = append(roots, root.ID)
				}
				return roots
			}

			// Look up child nodes for non-root IDs
			node := findNodeByID(id)
			if node == nil {
				return nil
			}

			var children []string
			for _, child := range node.Children {
				children = append(children, child.ID)
			}
			return children
		},
		func(id widget.TreeNodeID) bool {
			// Root nodes are branches if they have children
			if id == "" {
				return len(rootNodes) > 0
			}

			node := findNodeByID(id)
			return node != nil && len(node.Children) > 0
		},
		func(branch bool) fyne.CanvasObject {
			return container.NewHBox(
				widget.NewCheck("", nil),
				widget.NewIcon(nil),
				widget.NewLabel(""),
			)
		},
		func(id widget.TreeNodeID, branch bool, obj fyne.CanvasObject) {
			node := findNodeByID(id)
			if node == nil {
				return // Prevent nil pointer dereference
			}

			container := obj.(*fyne.Container)
			check := container.Objects[0].(*widget.Check)
			label := container.Objects[2].(*widget.Label)

			label.SetText(node.Name)
			check.Checked = node.Checked
			check.OnChanged = func(checked bool) {
				node.Checked = checked
				if checked {
					selectedNodes = append(selectedNodes, node)
				} else {
					removeNodeFromSelection(node)
				}
			}
		},
	)

	// Export button
	exportBtn := widget.NewButton("Generate Telegraf Config", func() {
		if len(selectedNodes) == 0 {
			dialog.ShowError(errors.New("no nodes selected"), mainWindow)
			return
		}

		cfg := generateConfig(endpoint)
		saveFileDialog := dialog.NewFileSave(func(w fyne.URIWriteCloser, err error) {
			if err != nil || w == nil {
				return
			}
			defer w.Close()
			_, err = w.Write([]byte(cfg))
			if err != nil {
				dialog.ShowError(err, mainWindow)
				return
			}
			dialog.ShowInformation("Success", "Config file saved successfully", mainWindow)
		}, mainWindow)

		saveFileDialog.SetFileName("telegraf.conf")
		saveFileDialog.Show()
	})

	return container.NewBorder(
		widget.NewLabel(fmt.Sprintf("Connected to: %s\nSelect nodes to monitor:", endpoint)),
		exportBtn,
		nil, nil,
		tree,
	)
}

func findNodeByID(id string) *NodeDef {
	var find func(nodes []*NodeDef) *NodeDef
	find = func(nodes []*NodeDef) *NodeDef {
		for _, n := range nodes {
			if n.ID == id {
				return n
			}
			if found := find(n.Children); found != nil {
				return found
			}
		}
		return nil
	}
	return find(rootNodes)
}

func removeNodeFromSelection(node *NodeDef) {
	for i, n := range selectedNodes {
		if n == node {
			selectedNodes = append(selectedNodes[:i], selectedNodes[i+1:]...)
			break
		}
	}
}

func generateConfig(endpoint string) string {
	config := map[string]interface{}{
		"agent": map[string]string{
			"interval": "10s",
		},
		"inputs": map[string]interface{}{
			"opcua": []map[string]interface{}{
				{
					"endpoint":        endpoint,
					"security_policy": "None",
					"auth_method":     "Anonymous",
					"nodes":           buildNodeConfig(),
				},
			},
		},
		"outputs": map[string]interface{}{
			"influxdb_v2": []map[string]interface{}{
				{
					"urls":         []string{"http://localhost:8086"},
					"token":        "YOUR_INFLUXDB_TOKEN",
					"organization": "YOUR_ORG",
					"bucket":       "YOUR_BUCKET",
				},
			},
		},
	}

	tomlBytes, err := toml.Marshal(config)
	if err != nil {
		log.Fatal(err)
	}
	return string(tomlBytes)
}

func buildNodeConfig() []map[string]interface{} {
	var nodes []map[string]interface{}
	for _, n := range selectedNodes {
		namespace, idType, identifier, err := parseNodeID(n.ID)
		if err != nil {
			log.Printf("skipping invalid node ID %q: %v", n.ID, err)
			continue
		}

		nodes = append(nodes, map[string]interface{}{
			"name":            n.Name,
			"namespace":       namespace,
			"identifier_type": idType,
			"identifier":      identifier,
		})
	}
	return nodes
}
