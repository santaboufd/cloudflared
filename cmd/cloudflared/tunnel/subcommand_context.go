package tunnel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/urfave/cli/v2"

	"github.com/cloudflare/cloudflared/certutil"
	"github.com/cloudflare/cloudflared/connection"
	"github.com/cloudflare/cloudflared/logger"
	"github.com/cloudflare/cloudflared/tunnelstore"
)

type errInvalidJSONCredential struct {
	err  error
	path string
}

func (e errInvalidJSONCredential) Error() string {
	return "Invalid JSON when parsing tunnel credentials file"
}

// subcommandContext carries structs shared between subcommands, to reduce number of arguments needed to
// pass between subcommands, and make sure they are only initialized once
type subcommandContext struct {
	c           *cli.Context
	log         *zerolog.Logger
	isUIEnabled bool
	fs          fileSystem

	// These fields should be accessed using their respective Getter
	tunnelstoreClient tunnelstore.Client
	userCredential    *userCredential
}

func newSubcommandContext(c *cli.Context) (*subcommandContext, error) {
	isUIEnabled := c.IsSet(uiFlag) && c.String("name") != ""

	// If UI is enabled, terminal log output should be disabled -- log should be written into a UI log window instead
	log := logger.CreateLoggerFromContext(c, isUIEnabled)

	return &subcommandContext{
		c:           c,
		log:         log,
		isUIEnabled: isUIEnabled,
		fs:          realFileSystem{},
	}, nil
}

// Returns something that can find the given tunnel's credentials file.
func (sc *subcommandContext) credentialFinder(tunnelID uuid.UUID) CredFinder {
	if path := sc.c.String(CredFileFlag); path != "" {
		return newStaticPath(path, sc.fs)
	}
	return newSearchByID(tunnelID, sc.c, sc.log, sc.fs)
}

type userCredential struct {
	cert     *certutil.OriginCert
	certPath string
}

func (sc *subcommandContext) client() (tunnelstore.Client, error) {
	if sc.tunnelstoreClient != nil {
		return sc.tunnelstoreClient, nil
	}
	credential, err := sc.credential()
	if err != nil {
		return nil, err
	}
	userAgent := fmt.Sprintf("cloudflared/%s", version)
	client, err := tunnelstore.NewRESTClient(
		sc.c.String("api-url"),
		credential.cert.AccountID,
		credential.cert.ZoneID,
		credential.cert.ServiceKey,
		userAgent,
		sc.log,
	)

	if err != nil {
		return nil, err
	}
	sc.tunnelstoreClient = client
	return client, nil
}

func (sc *subcommandContext) credential() (*userCredential, error) {
	if sc.userCredential == nil {
		originCertPath := sc.c.String("origincert")
		originCertLog := sc.log.With().
			Str(LogFieldOriginCertPath, originCertPath).
			Logger()

		originCertPath, err := findOriginCert(originCertPath, &originCertLog)
		if err != nil {
			return nil, errors.Wrap(err, "Error locating origin cert")
		}
		blocks, err := readOriginCert(originCertPath)
		if err != nil {
			return nil, errors.Wrapf(err, "Can't read origin cert from %s", originCertPath)
		}

		cert, err := certutil.DecodeOriginCert(blocks)
		if err != nil {
			return nil, errors.Wrap(err, "Error decoding origin cert")
		}

		if cert.AccountID == "" {
			return nil, errors.Errorf(`Origin certificate needs to be refreshed before creating new tunnels.\nDelete %s and run "cloudflared login" to obtain a new cert.`, originCertPath)
		}

		sc.userCredential = &userCredential{
			cert:     cert,
			certPath: originCertPath,
		}
	}
	return sc.userCredential, nil
}

func (sc *subcommandContext) readTunnelCredentials(credFinder CredFinder) (connection.Credentials, error) {
	filePath, err := credFinder.Path()
	if err != nil {
		return connection.Credentials{}, err
	}
	body, err := sc.fs.readFile(filePath)
	if err != nil {
		return connection.Credentials{}, errors.Wrapf(err, "couldn't read tunnel credentials from %v", filePath)
	}

	var credentials connection.Credentials
	if err = json.Unmarshal(body, &credentials); err != nil {
		if strings.HasSuffix(filePath, ".pem") {
			return connection.Credentials{}, fmt.Errorf("The tunnel credentials file should be .json but you gave a .pem. " +
				"The tunnel credentials file was originally created by `cloudflared tunnel create`. " +
				"You may have accidentally used the filepath to cert.pem, which is generated by `cloudflared tunnel " +
				"login`.")
		}
		return connection.Credentials{}, errInvalidJSONCredential{path: filePath, err: err}
	}
	return credentials, nil
}

func (sc *subcommandContext) create(name string, credentialsFilePath string) (*tunnelstore.Tunnel, error) {
	client, err := sc.client()
	if err != nil {
		return nil, errors.Wrap(err, "couldn't create client to talk to Cloudflare Tunnel backend")
	}

	tunnelSecret, err := generateTunnelSecret()
	if err != nil {
		return nil, errors.Wrap(err, "couldn't generate the secret for your new tunnel")
	}

	tunnel, err := client.CreateTunnel(name, tunnelSecret)
	if err != nil {
		return nil, errors.Wrap(err, "Create Tunnel API call failed")
	}

	credential, err := sc.credential()
	if err != nil {
		return nil, err
	}
	tunnelCredentials := connection.Credentials{
		AccountTag:   credential.cert.AccountID,
		TunnelSecret: tunnelSecret,
		TunnelID:     tunnel.ID,
		TunnelName:   name,
	}
	usedCertPath := false
	if credentialsFilePath == "" {
		originCertDir := filepath.Dir(credential.certPath)
		credentialsFilePath, err = tunnelFilePath(tunnelCredentials.TunnelID, originCertDir)
		if err != nil {
			return nil, err
		}
		usedCertPath = true
	}
	writeFileErr := writeTunnelCredentials(credentialsFilePath, &tunnelCredentials)
	if writeFileErr != nil {
		var errorLines []string
		errorLines = append(errorLines, fmt.Sprintf("Your tunnel '%v' was created with ID %v. However, cloudflared couldn't write tunnel credentials to %s.", tunnel.Name, tunnel.ID, credentialsFilePath))
		errorLines = append(errorLines, fmt.Sprintf("The file-writing error is: %v", writeFileErr))
		if deleteErr := client.DeleteTunnel(tunnel.ID); deleteErr != nil {
			errorLines = append(errorLines, fmt.Sprintf("Cloudflared tried to delete the tunnel for you, but encountered an error. You should use `cloudflared tunnel delete %v` to delete the tunnel yourself, because the tunnel can't be run without the tunnelfile.", tunnel.ID))
			errorLines = append(errorLines, fmt.Sprintf("The delete tunnel error is: %v", deleteErr))
		} else {
			errorLines = append(errorLines, fmt.Sprintf("The tunnel was deleted, because the tunnel can't be run without the credentials file"))
		}
		errorMsg := strings.Join(errorLines, "\n")
		return nil, errors.New(errorMsg)
	}

	if outputFormat := sc.c.String(outputFormatFlag.Name); outputFormat != "" {
		return nil, renderOutput(outputFormat, &tunnel)
	}

	fmt.Printf("Tunnel credentials written to %v.", credentialsFilePath)
	if usedCertPath {
		fmt.Print(" cloudflared chose this file based on where your origin certificate was found.")
	}
	fmt.Println(" Keep this file secret. To revoke these credentials, delete the tunnel.")
	fmt.Printf("\nCreated tunnel %s with id %s\n", tunnel.Name, tunnel.ID)
	return tunnel, nil
}

func (sc *subcommandContext) list(filter *tunnelstore.Filter) ([]*tunnelstore.Tunnel, error) {
	client, err := sc.client()
	if err != nil {
		return nil, err
	}
	return client.ListTunnels(filter)
}

func (sc *subcommandContext) delete(tunnelIDs []uuid.UUID) error {
	forceFlagSet := sc.c.Bool("force")

	client, err := sc.client()
	if err != nil {
		return err
	}

	for _, id := range tunnelIDs {
		tunnel, err := client.GetTunnel(id)
		if err != nil {
			return errors.Wrapf(err, "Can't get tunnel information. Please check tunnel id: %s", tunnel.ID)
		}

		// Check if tunnel DeletedAt field has already been set
		if !tunnel.DeletedAt.IsZero() {
			return fmt.Errorf("Tunnel %s has already been deleted", tunnel.ID)
		}
		if forceFlagSet {
			if err := client.CleanupConnections(tunnel.ID, tunnelstore.NewCleanupParams()); err != nil {
				return errors.Wrapf(err, "Error cleaning up connections for tunnel %s", tunnel.ID)
			}
		}

		if err := client.DeleteTunnel(tunnel.ID); err != nil {
			return errors.Wrapf(err, "Error deleting tunnel %s", tunnel.ID)
		}

		credFinder := sc.credentialFinder(id)
		if tunnelCredentialsPath, err := credFinder.Path(); err == nil {
			if err = os.Remove(tunnelCredentialsPath); err != nil {
				sc.log.Info().Msgf("Tunnel %v was deleted, but we could not remove its credentials file  %s: %s. Consider deleting this file manually.", id, tunnelCredentialsPath, err)
			}
		}
	}
	return nil
}

// findCredentials will choose the right way to find the credentials file, find it,
// and add the TunnelID into any old credentials (generated before TUN-3581 added the `TunnelID`
// field to credentials files)
func (sc *subcommandContext) findCredentials(tunnelID uuid.UUID) (connection.Credentials, error) {
	var credentials connection.Credentials
	var err error
	if credentialsContents := sc.c.String(CredContentsFlag); credentialsContents != "" {
		if err = json.Unmarshal([]byte(credentialsContents), &credentials); err != nil {
			err = errInvalidJSONCredential{path: "TUNNEL_CRED_CONTENTS", err: err}
		}
	} else {
		credFinder := sc.credentialFinder(tunnelID)
		credentials, err = sc.readTunnelCredentials(credFinder)
	}
	// This line ensures backwards compatibility with credentials files generated before
	// TUN-3581. Those old credentials files don't have a TunnelID field, so we enrich the struct
	// with the ID, which we have already resolved from the user input.
	credentials.TunnelID = tunnelID
	return credentials, err
}

func (sc *subcommandContext) run(tunnelID uuid.UUID) error {
	credentials, err := sc.findCredentials(tunnelID)
	if err != nil {
		if e, ok := err.(errInvalidJSONCredential); ok {
			sc.log.Error().Msgf("The credentials file at %s contained invalid JSON. This is probably caused by passing the wrong filepath. Reminder: the credentials file is a .json file created via `cloudflared tunnel create`.", e.path)
			sc.log.Error().Msgf("Invalid JSON when parsing credentials file: %s", e.err.Error())
		}
		return err
	}

	return StartServer(
		sc.c,
		version,
		&connection.NamedTunnelConfig{Credentials: credentials},
		sc.log,
		sc.isUIEnabled,
	)
}

func (sc *subcommandContext) cleanupConnections(tunnelIDs []uuid.UUID) error {
	params := tunnelstore.NewCleanupParams()
	extraLog := ""
	if connector := sc.c.String("connector-id"); connector != "" {
		connectorID, err := uuid.Parse(connector)
		if err != nil {
			return errors.Wrapf(err, "%s is not a valid client ID (must be a UUID)", connector)
		}
		params.ForClient(connectorID)
		extraLog = fmt.Sprintf(" for connector-id %s", connectorID.String())
	}

	client, err := sc.client()
	if err != nil {
		return err
	}
	for _, tunnelID := range tunnelIDs {
		sc.log.Info().Msgf("Cleanup connection for tunnel %s%s", tunnelID, extraLog)
		if err := client.CleanupConnections(tunnelID, params); err != nil {
			sc.log.Error().Msgf("Error cleaning up connections for tunnel %v, error :%v", tunnelID, err)
		}
	}
	return nil
}

func (sc *subcommandContext) route(tunnelID uuid.UUID, r tunnelstore.Route) (tunnelstore.RouteResult, error) {
	client, err := sc.client()
	if err != nil {
		return nil, err
	}

	return client.RouteTunnel(tunnelID, r)
}

// Query Tunnelstore to find the active tunnel with the given name.
func (sc *subcommandContext) tunnelActive(name string) (*tunnelstore.Tunnel, bool, error) {
	filter := tunnelstore.NewFilter()
	filter.NoDeleted()
	filter.ByName(name)
	tunnels, err := sc.list(filter)
	if err != nil {
		return nil, false, err
	}
	if len(tunnels) == 0 {
		return nil, false, nil
	}
	// There should only be 1 active tunnel for a given name
	return tunnels[0], true, nil
}

// findID parses the input. If it's a UUID, return the UUID.
// Otherwise, assume it's a name, and look up the ID of that tunnel.
func (sc *subcommandContext) findID(input string) (uuid.UUID, error) {
	if u, err := uuid.Parse(input); err == nil {
		return u, nil
	}

	// Look up name in the credentials file.
	credFinder := newStaticPath(sc.c.String(CredFileFlag), sc.fs)
	if credentials, err := sc.readTunnelCredentials(credFinder); err == nil {
		if credentials.TunnelID != uuid.Nil && input == credentials.TunnelName {
			return credentials.TunnelID, nil
		}
	}

	// Fall back to querying Tunnelstore.
	if tunnel, found, err := sc.tunnelActive(input); err != nil {
		return uuid.Nil, err
	} else if found {
		return tunnel.ID, nil
	}

	return uuid.Nil, fmt.Errorf("%s is neither the ID nor the name of any of your tunnels", input)
}

// findIDs is just like mapping `findID` over a slice, but it only uses
// one Tunnelstore API call.
func (sc *subcommandContext) findIDs(inputs []string) ([]uuid.UUID, error) {

	// Shortcut without Tunnelstore call if we find that all inputs are already UUIDs.
	uuids, err := convertNamesToUuids(inputs, make(map[string]uuid.UUID))
	if err == nil {
		return uuids, nil
	}

	// First, look up all tunnels the user has
	filter := tunnelstore.NewFilter()
	filter.NoDeleted()
	tunnels, err := sc.list(filter)
	if err != nil {
		return nil, err
	}
	// Do the pure list-processing in its own function, so that it can be
	// unit tested easily.
	return findIDs(tunnels, inputs)
}

func findIDs(tunnels []*tunnelstore.Tunnel, inputs []string) ([]uuid.UUID, error) {
	// Put them into a dictionary for faster lookups
	nameToID := make(map[string]uuid.UUID, len(tunnels))
	for _, tunnel := range tunnels {
		nameToID[tunnel.Name] = tunnel.ID
	}

	return convertNamesToUuids(inputs, nameToID)
}

func convertNamesToUuids(inputs []string, nameToID map[string]uuid.UUID) ([]uuid.UUID, error) {
	tunnelIDs := make([]uuid.UUID, len(inputs))
	var badInputs []string
	for i, input := range inputs {
		if id, err := uuid.Parse(input); err == nil {
			tunnelIDs[i] = id
		} else if id, ok := nameToID[input]; ok {
			tunnelIDs[i] = id
		} else {
			badInputs = append(badInputs, input)
		}
	}
	if len(badInputs) > 0 {
		msg := "Please specify either the ID or name of a tunnel. The following inputs were neither: %s"
		return nil, fmt.Errorf(msg, strings.Join(badInputs, ", "))
	}
	return tunnelIDs, nil
}
