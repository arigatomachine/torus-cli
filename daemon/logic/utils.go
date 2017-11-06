package logic

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"time"

	"golang.org/x/crypto/ed25519"

	"github.com/manifoldco/go-base64"

	"github.com/manifoldco/torus-cli/apitypes"
	"github.com/manifoldco/torus-cli/envelope"
	"github.com/manifoldco/torus-cli/identity"
	"github.com/manifoldco/torus-cli/primitive"
	"github.com/manifoldco/torus-cli/registry"

	"github.com/manifoldco/torus-cli/daemon/crypto"
	"github.com/manifoldco/torus-cli/daemon/session"
)

func packageSigningKeypair(ctx context.Context, c *crypto.Engine, authID, orgID *identity.ID,
	kp *crypto.KeyPairs) (*envelope.PublicKey, *envelope.PrivateKey, error) {

	pubsig, err := packagePublicKey(ctx, c, authID, orgID,
		primitive.SigningKeyType, kp.Signature.Public, nil, &kp.Signature)
	if err != nil {
		return nil, nil, err
	}

	privsig, err := packagePrivateKey(ctx, c, authID, orgID, kp.Signature.PNonce,
		kp.Signature.Private, pubsig.ID, pubsig.ID, &kp.Signature)
	if err != nil {
		return nil, nil, err
	}

	return pubsig, privsig, nil
}

func packageEncryptionKeypair(ctx context.Context, c *crypto.Engine, authID, orgID *identity.ID,
	kp *crypto.KeyPairs, pubsig *envelope.PublicKey) (*envelope.PublicKey, *envelope.PrivateKey, error) {

	pubenc, err := packagePublicKey(ctx, c, authID, orgID, primitive.EncryptionKeyType,
		kp.Encryption.Public[:], pubsig.ID, &kp.Signature)
	if err != nil {
		return nil, nil, err
	}

	privenc, err := packagePrivateKey(ctx, c, authID, orgID, kp.Encryption.PNonce,
		kp.Encryption.Private, pubenc.ID, pubsig.ID, &kp.Signature)
	if err != nil {
		return nil, nil, err
	}

	return pubenc, privenc, nil
}

// createCredentialGraph generates, signs, and posts a new CredentialGraph
// to the registry.
func createCredentialGraph(ctx context.Context, credBody *PlaintextCredential,
	parent registry.CredentialGraph, sigID *identity.ID, encID *identity.ID, kp *crypto.KeyPairs,
	client *registry.Client, engine *crypto.Engine) (*registry.CredentialGraphV2, error) {

	pathExp, err := credBody.PathExp.WithInstance("*")
	if err != nil {
		return nil, err
	}

	keyringBody := primitive.NewKeyring(credBody.OrgID, credBody.ProjectID, pathExp)
	if parent != nil {
		keyringBody.Previous = parent.GetKeyring().GetID()
		keyringBody.KeyringVersion = parent.KeyringVersion() + 1
	}

	keyring, err := engine.SignedKeyring(ctx, keyringBody, sigID, &kp.Signature)
	if err != nil {
		return nil, err
	}

	// XXX: sensitive value. protect with OS things.
	mek := make([]byte, 64)
	_, err = rand.Read(mek)
	if err != nil {
		return nil, err
	}

	subjects, err := getKeyringMembers(ctx, client, credBody.OrgID)
	if err != nil {
		return nil, err
	}

	claimTrees, err := client.ClaimTree.List(ctx, credBody.OrgID, nil)
	if err != nil {
		return nil, err
	}

	if len(claimTrees) != 1 {
		return nil, &apitypes.Error{
			Type: apitypes.NotFoundError,
			Err: []string{
				fmt.Sprintf("Claim tree not found for org: %s", credBody.OrgID),
			},
		}
	}

	// use their public key to encrypt the mek with a random nonce.
	members := []registry.KeyringMember{}
	for _, subject := range subjects {
		for _, id := range subject.KeyOwnerIDs() {
			// For this user/mtoken, find their public encryption key
			encPubKey, err := findEncryptionPublicKey(claimTrees, credBody.OrgID, &id)
			if err != nil {
				// If we didn't find an active key, don't encode this user/token
				// in the keyring, but keep going.
				continue
			}

			encmek, nonce, err := engine.Box(ctx, mek, &kp.Encryption, []byte(*encPubKey.Body.Key.Value))
			if err != nil {
				return nil, err
			}

			key := &primitive.KeyringMemberKey{
				Algorithm: crypto.EasyBox,
				Nonce:     base64.New(nonce),
				Value:     base64.New(encmek),
			}

			member, err := newV2KeyringMember(ctx, engine, credBody.OrgID, keyring.ID,
				encPubKey.Body.OwnerID, encPubKey.ID, encID, sigID, key, kp)
			if err != nil {
				return nil, err
			}

			members = append(members, *member)
		}
	}

	graph := registry.CredentialGraphV2{
		KeyringSectionV2: registry.KeyringSectionV2{
			Keyring: keyring,
			Claims:  []envelope.KeyringMemberClaim{},
			Members: members,
		},
	}

	return &graph, nil
}

func newV1KeyringMember(ctx context.Context, engine *crypto.Engine,
	orgID, projectID, keyringID, ownerID, pubKeyID, encKeyID, sigID *identity.ID,
	key *primitive.KeyringMemberKey, kp *crypto.KeyPairs) (*envelope.KeyringMemberV1, error) {

	now := time.Now().UTC()
	return engine.SignedKeyringMemberV1(ctx, &primitive.KeyringMemberV1{
		Created:         now,
		OrgID:           orgID,
		ProjectID:       projectID,
		KeyringID:       keyringID,
		OwnerID:         ownerID,
		PublicKeyID:     pubKeyID,
		EncryptingKeyID: encKeyID,
		Key:             key,
	}, sigID, &kp.Signature)
}

func newV2KeyringMember(ctx context.Context, engine *crypto.Engine,
	orgID, keyringID, ownerID, pubKeyID, encKeyID, sigID *identity.ID,
	key *primitive.KeyringMemberKey, kp *crypto.KeyPairs) (*registry.KeyringMember, error) {

	now := time.Now().UTC()
	member, err := engine.SignedKeyringMember(ctx, &primitive.KeyringMember{
		Created:         now,
		OrgID:           orgID,
		KeyringID:       keyringID,
		OwnerID:         ownerID,
		PublicKeyID:     pubKeyID,
		EncryptingKeyID: encKeyID,
	}, sigID, &kp.Signature)

	if err != nil {
		return nil, err
	}

	mekshare, err := engine.SignedMEKShare(ctx, &primitive.MEKShare{
		Created:         now,
		OrgID:           orgID,
		OwnerID:         ownerID,
		KeyringID:       keyringID,
		KeyringMemberID: member.ID,
		Key:             key,
	}, sigID, &kp.Signature)
	if err != nil {
		return nil, err
	}

	return &registry.KeyringMember{
		Member:   member,
		MEKShare: mekshare,
	}, nil
}

func createKeyringMemberships(ctx context.Context, c *crypto.Engine, client *registry.Client,
	s session.Session, orgID, ownerID *identity.ID) ([]envelope.KeyringMemberV1, []registry.KeyringMember, error) {

	// Get this user's keypairs
	sigID, encID, kp, err := fetchKeyPairs(ctx, client, orgID)
	if err != nil {
		log.Printf("could not fetch keypairs for org: %s", err)
		return nil, nil, err
	}

	claimTrees, err := client.ClaimTree.List(ctx, orgID, nil)
	if err != nil {
		log.Printf("could not retrieve claim tree for invite approval: %s", err)
		return nil, nil, err
	}

	if len(claimTrees) != 1 {
		log.Printf("incorrect number of claim trees returned: %d", len(claimTrees))
		return nil, nil, &apitypes.Error{
			Type: apitypes.NotFoundError,
			Err: []string{
				fmt.Sprintf("Claim tree not found for org: %s", orgID),
			},
		}
	}

	// Get all the keyrings and memberships for the current user. This way we
	// can decrypt the MEK for each and then create a new KeyringMember for
	// our wonderful new org member!
	org, err := client.Orgs.Get(ctx, orgID)
	if err != nil {
		return nil, nil, err
	}

	projects, err := client.Projects.List(ctx, org.ID)
	if err != nil {
		return nil, nil, err
	}

	var graphs []registry.CredentialGraph
	for _, project := range projects {
		projGraphs, err := client.CredentialGraph.Search(ctx,
			"/"+org.Body.Name+"/"+project.Body.Name+"/*/*/*/*", s.AuthID())
		if err != nil {
			log.Printf("Error retrieving credential graphs: %s", err)
			return nil, nil, err
		}

		graphs = append(graphs, projGraphs...)
	}

	// Find encryption keys for user
	targetPubKey, err := findEncryptionPublicKey(claimTrees, orgID, ownerID)
	if err != nil {
		log.Printf("could not find encryption key for owner id: %s", ownerID.String())
		return nil, nil, err
	}

	cgs := newCredentialGraphSet()
	err = cgs.Add(graphs...)
	if err != nil {
		return nil, nil, err
	}

	activeGraphs, err := cgs.Active()
	if err != nil {
		return nil, nil, err
	}

	v1members := []envelope.KeyringMemberV1{}
	v2members := []registry.KeyringMember{}
	for _, graph := range activeGraphs {
		krm, mekshare, err := graph.FindMember(s.AuthID())
		if err != nil {
			log.Printf("could not find keyring membership: %s", err)
			return nil, nil, &apitypes.Error{
				Type: apitypes.NotFoundError,
				Err:  []string{"Keyring membership not found."},
			}
		}

		encPubKey, err := findEncryptionPublicKeyByID(claimTrees, orgID, krm.EncryptingKeyID)
		if err != nil {
			log.Printf("could not find encypting public key for membership: %s", err)
			return nil, nil, err
		}

		encMek, nonce, err := c.CloneMembership(ctx, *mekshare.Key.Value,
			*mekshare.Key.Nonce, &kp.Encryption, *encPubKey.Body.Key.Value, *targetPubKey.Body.Key.Value)
		if err != nil {
			log.Printf("could not clone keyring membership: %s", err)
			return nil, nil, err
		}

		key := &primitive.KeyringMemberKey{
			Algorithm: crypto.EasyBox,
			Nonce:     base64.New(nonce),
			Value:     base64.New(encMek),
		}

		switch k := graph.GetKeyring().(type) {
		case *envelope.KeyringV1:
			projectID := k.Body.ProjectID
			member, err := newV1KeyringMember(ctx, c, krm.OrgID, projectID,
				krm.KeyringID, ownerID, targetPubKey.ID, encID, sigID, key, kp)
			if err != nil {
				return nil, nil, err
			}
			v1members = append(v1members, *member)
		case *envelope.Keyring:
			member, err := newV2KeyringMember(ctx, c, krm.OrgID, krm.KeyringID,
				ownerID, targetPubKey.ID, encID, sigID, key, kp)
			if err != nil {
				return nil, nil, err
			}
			v2members = append(v2members, *member)
		default:
			return nil, nil, &apitypes.Error{
				Type: apitypes.InternalServerError,
				Err:  []string{"Unknown keyring schema version"},
			}
		}
	}

	return v1members, v2members, nil
}

// fetchRegistryKeyPairs fetches the user's signing and encryption keypairs
// from the registry for the given org id.
// It returns an error if the keypairs cannot be fetched from the registry,
// or if an invalid keypair type is seen.
// It returns nil for keypair types that are not found.
func fetchRegistryKeyPairs(ctx context.Context, client *registry.Client,
	orgID *identity.ID) (*registry.ClaimedKeyPair, *registry.ClaimedKeyPair, error) {

	keyPairs, err := client.KeyPairs.List(ctx, orgID)
	if err != nil {
		return nil, nil, err
	}

	var sigClaimed *registry.ClaimedKeyPair
	var encClaimed *registry.ClaimedKeyPair
	for _, kp := range keyPairs {
		var keyPair = kp
		if keyPair.Revoked() {
			continue
		}

		switch kt := keyPair.PublicKey.Body.KeyType; kt {
		case primitive.SigningKeyType:
			sigClaimed = &keyPair
		case primitive.EncryptionKeyType:
			encClaimed = &keyPair
		default:
			return nil, nil, &apitypes.Error{
				Type: apitypes.InternalServerError,
				Err:  []string{fmt.Sprintf("Unknown key type: %s", kt)},
			}
		}
	}

	return encClaimed, sigClaimed, nil
}

// fetchKeyPairs fetches the user's signing and encryption keypairs from the
// registry for the given org id.
func fetchKeyPairs(ctx context.Context, client *registry.Client,
	orgID *identity.ID) (*identity.ID, *identity.ID, *crypto.KeyPairs, error) {

	encClaimed, sigClaimed, err := fetchRegistryKeyPairs(ctx, client, orgID)
	if err != nil {
		return nil, nil, nil, err
	}

	if sigClaimed == nil || encClaimed == nil {
		return nil, nil, nil, &apitypes.Error{
			Type: apitypes.NotFoundError,
			Err:  []string{"Missing encryption or signing keypairs"},
		}
	}

	kp := bundleKeypairs(sigClaimed, encClaimed)
	return sigClaimed.PublicKey.ID, encClaimed.PublicKey.ID, kp, nil
}

func bundleKeypairs(sigClaimed, encClaimed *registry.ClaimedKeyPair) *crypto.KeyPairs {

	sigPub := sigClaimed.PublicKey.Body.Key.Value
	sigKP := crypto.SignatureKeyPair{
		Public:  ed25519.PublicKey(*sigPub),
		Private: *sigClaimed.PrivateKey.Body.Key.Value,
		PNonce:  *sigClaimed.PrivateKey.Body.PNonce,
	}

	kp := crypto.KeyPairs{
		Signature: sigKP,
	}

	if encClaimed != nil {
		encPub := *encClaimed.PublicKey.Body.Key.Value
		encPubB := [32]byte{}
		copy(encPubB[:], encPub)
		kp.Encryption = crypto.EncryptionKeyPair{
			Public:  encPubB,
			Private: *encClaimed.PrivateKey.Body.Key.Value,
			PNonce:  *encClaimed.PrivateKey.Body.PNonce,
		}
	}

	return &kp
}

// findEncryptingKey queries the registry for public keys in the given org, to
// find the matching one
func findEncryptingKey(ctx context.Context, client *registry.Client, orgID *identity.ID,
	encryptingKeyID *identity.ID) (*primitive.PublicKey, error) {

	claimTrees, err := client.ClaimTree.List(ctx, orgID, nil)
	if err != nil {
		return nil, err
	}

	if len(claimTrees) != 1 {
		return nil, &apitypes.Error{
			Type: apitypes.NotFoundError,
			Err: []string{
				fmt.Sprintf("Claim tree not found for org: %s", orgID),
			},
		}
	}

	var encryptingKey *primitive.PublicKey
	for _, segment := range claimTrees[0].PublicKeys {
		if *segment.PublicKey.ID == *encryptingKeyID {
			encryptingKey = segment.PublicKey.Body
			break
		}
	}
	if encryptingKey == nil {
		return nil, &apitypes.Error{
			Type: apitypes.NotFoundError,
			Err: []string{
				fmt.Sprintf("Encrypting key not found: %s", encryptingKeyID),
			},
		}
	}

	return encryptingKey, nil
}

// findSystemTeams takes in a list of team objects and returns the members and machines
// teams.
func findSystemTeams(teams []envelope.Team) (*envelope.Team, *envelope.Team, error) {
	var members, machines *envelope.Team
	for _, t := range teams {
		var team = t
		if t.Body.TeamType == primitive.SystemTeamType {
			switch t.Body.Name {
			case "member":
				members = &team
			case "machine":
				machines = &team
			}
		}

		if members != nil && machines != nil {
			break
		}
	}

	var errs []string
	if members == nil {
		errs = append(errs, "Member team not found.")
	}
	if machines == nil {
		errs = append(errs, "Machine team not found.")
	}

	if len(errs) > 0 {
		return nil, nil, &apitypes.Error{
			Err:  errs,
			Type: apitypes.NotFoundError,
		}
	}

	return members, machines, nil
}

func findEncryptionPublicKey(trees []registry.ClaimTree, orgID *identity.ID,
	userID *identity.ID) (*envelope.PublicKey, error) {

	// Loop over claimtree looking for the users encryption key
	var encKey *envelope.PublicKey
	for _, tree := range trees {
		if *tree.Org.ID != *orgID {
			continue
		}

		for _, segment := range tree.PublicKeys {
			if segment.Revoked() {
				continue
			}

			key := segment.PublicKey
			if *key.Body.OwnerID != *userID {
				continue
			}

			if key.Body.KeyType != primitive.EncryptionKeyType {
				continue
			}

			encKey = key
		}
	}

	if encKey == nil {
		err := fmt.Errorf("No encryption pubkey found for: %s", userID.String())
		return nil, err
	}

	return encKey, nil
}

func findEncryptionPublicKeyByID(trees []registry.ClaimTree, orgID *identity.ID,
	ID *identity.ID) (*envelope.PublicKey, error) {

	// Loop over claimtree looking for the users encryption key
	var encKey *envelope.PublicKey
	for _, tree := range trees {
		if *tree.Org.ID != *orgID {
			continue
		}

		for _, segment := range tree.PublicKeys {
			if segment.Revoked() {
				continue
			}

			key := segment.PublicKey
			if *key.ID != *ID {
				continue
			}

			if key.Body.KeyType != primitive.EncryptionKeyType {
				continue
			}

			encKey = key
		}
	}

	if encKey == nil {
		return nil, &apitypes.Error{
			Type: apitypes.NotFoundError,
			Err: []string{
				fmt.Sprintf("Encryption pubkey not found for: %s", ID.String()),
			},
		}
	}

	return encKey, nil
}

func packagePublicKey(ctx context.Context, engine *crypto.Engine, ownerID,
	orgID *identity.ID, keyType primitive.KeyType, public []byte, sigID *identity.ID,
	sigKP *crypto.SignatureKeyPair) (*envelope.PublicKey, error) {

	alg := crypto.Curve25519
	if keyType == primitive.SigningKeyType {
		alg = crypto.EdDSA
	}

	now := time.Now().UTC()

	body := primitive.PublicKey{
		OrgID:     orgID,
		OwnerID:   ownerID,
		KeyType:   keyType,
		Algorithm: alg,

		Key: primitive.PublicKeyValue{
			Value: base64.New(public),
		},

		Created: now,
		Expires: now.Add(time.Hour * 8760), // one year
	}

	return engine.SignedPublicKey(ctx, &body, sigID, sigKP)
}

func packagePrivateKey(ctx context.Context, engine *crypto.Engine, ownerID,
	orgID *identity.ID, pnonce, private []byte, pubID, sigID *identity.ID,
	sigKP *crypto.SignatureKeyPair) (*envelope.PrivateKey, error) {

	body := primitive.PrivateKey{
		OrgID:       orgID,
		OwnerID:     ownerID,
		PNonce:      base64.New(pnonce),
		PublicKeyID: pubID,

		Key: primitive.PrivateKeyValue{
			Algorithm: crypto.Triplesec,
			Value:     base64.New(private),
		},
	}

	return engine.SignedPrivateKey(ctx, &body, sigID, sigKP)
}

// keyringMember is the interface used to abstract user vs machine pubkey
// ownership. A user directly owns their pubkeys, whereas a machine owns 0 or
// more tokens that own pubkeys.
type keyringMember interface {
	// The ID of the user/machine
	GetID() *identity.ID

	// The IDs of either the user itself, or the machine's tokens.
	KeyOwnerIDs() []identity.ID
}

type userKeyringMember struct {
	id *identity.ID
}

func (m *userKeyringMember) GetID() *identity.ID {
	return m.id
}

func (m *userKeyringMember) KeyOwnerIDs() []identity.ID {
	return []identity.ID{*m.id}
}

type machineKeyringMember struct {
	*envelope.Machine
	tokens []identity.ID
}

func (m *machineKeyringMember) KeyOwnerIDs() []identity.ID {
	return m.tokens
}

// getKeyringMembers returns a slice of keyringMembers of all subjects that
// should be members of a keyring.
// This includes both users and machines.
// XXX: we need to filter the members down based on ACL
func getKeyringMembers(ctx context.Context, client *registry.Client,
	orgID *identity.ID) ([]keyringMember, error) {

	teams, err := client.Teams.GetByOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}

	membersTeam, machinesTeam, err := findSystemTeams(teams)
	if err != nil {
		return nil, err
	}

	userMembers, err := client.Memberships.List(ctx, orgID, membersTeam.ID, nil)
	if err != nil {
		return nil, err
	}

	machineMembers, err := client.Memberships.List(ctx, orgID, machinesTeam.ID, nil)
	if err != nil {
		return nil, err
	}

	var members []keyringMember
	for _, membership := range userMembers {
		members = append(members, &userKeyringMember{id: membership.Body.OwnerID})
	}

	for _, membership := range machineMembers {
		machineID := membership.Body.OwnerID
		segment, err := client.Machines.Get(ctx, machineID)
		if err != nil {
			return nil, err
		}

		m := machineKeyringMember{Machine: segment.Machine}
		for _, token := range segment.Tokens {
			if token.Token.Body.State == primitive.MachineTokenActiveState {
				m.tokens = append(m.tokens, *token.Token.ID)
			}
		}
		members = append(members, &m)
	}

	return members, nil
}

func ignoreUnset(creds []envelope.CredentialInf) []envelope.CredentialInf {
	out := []envelope.CredentialInf{}
	for _, c := range creds {
		if c.Unset() {
			continue
		}

		out = append(out, c)
	}

	return out
}
