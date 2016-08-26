package routes

// The file contains routes related to org invitations

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/go-zoo/bone"

	"github.com/arigatomachine/cli/apitypes"

	"github.com/arigatomachine/cli/daemon/base64"
	"github.com/arigatomachine/cli/daemon/crypto"
	"github.com/arigatomachine/cli/daemon/db"
	"github.com/arigatomachine/cli/daemon/envelope"
	"github.com/arigatomachine/cli/daemon/identity"
	"github.com/arigatomachine/cli/daemon/observer"
	"github.com/arigatomachine/cli/daemon/primitive"
	"github.com/arigatomachine/cli/daemon/registry"
	"github.com/arigatomachine/cli/daemon/session"
)

func orgInvitesApproveRoute(client *registry.Client, s session.Session,
	db *db.DB, engine *crypto.Engine, o *observer.Observer) http.HandlerFunc {

	var steps uint = 6

	return func(w http.ResponseWriter, r *http.Request) {
		var step uint = 1
		ctx := r.Context()
		inviteID, err := identity.DecodeFromString(bone.GetValue(r, "id"))
		if err != nil {
			log.Printf("Could not approve org invite; invalid id: %s", err)
			encodeResponseErr(w, err)
			return
		}

		// Get the invite!
		invite, err := client.OrgInvite.Get(ctx, &inviteID)
		if err != nil {
			log.Printf("could not fetch org invitation: %s", err)
			encodeResponseErr(w, err)
			return
		}

		inviteBody := invite.Body.(*primitive.OrgInvite)

		enc := json.NewEncoder(w)
		if inviteBody.State != primitive.OrgInviteAcceptedState {
			log.Printf("invitation not in accepted state: %s", inviteBody.State)
			err = enc.Encode(&errorMsg{
				Type:  apitypes.BadRequestError,
				Error: []string{"Invite must be accepted before it can be approved"},
			})
			if err != nil {
				encodeResponseErr(w, err)
			}
			return
		}

		o.Notify(ctx, observer.Progress, "Invite retrieved", step, steps)
		step++

		// Get this users keypairs
		sigID, encID, kp, err := fetchKeyPairs(ctx, client, inviteBody.OrgID)
		if err != nil {
			log.Printf("could not fetch keypairs for org: %s", err)
			encodeResponseErr(w, err)
			return
		}

		o.Notify(ctx, observer.Progress, "Keypairs retrieved", step, steps)
		step++

		claimTrees, err := client.ClaimTree.List(r.Context(), inviteBody.OrgID, nil)
		if err != nil {
			log.Printf("could not retrieve claim tree for invite approval: %s", err)
			encodeResponseErr(w, err)
			return
		}

		if len(claimTrees) != 1 {
			log.Printf("incorrect number of claim trees returned: %d", len(claimTrees))
			encodeResponseErr(w, fmt.Errorf(
				"No claim tree found for org: %s", inviteBody.OrgID))
			return
		}

		o.Notify(ctx, observer.Progress, "Claims retrieved", step, steps)
		step++

		// Get all the keyrings and memberships for the current user. This way we
		// can decrypt the MEK for each and then create a new KeyringMember for
		// our wonderful new org member!
		keyringSections, err := client.Keyring.List(ctx, inviteBody.OrgID, s.ID())
		if err != nil {
			log.Printf("could not retrieve keyring sections for user: %s", err)
			encodeResponseErr(w, err)
			return
		}

		// Find encryption keys for user
		targetPubKey, err := findEncryptionPublicKey(claimTrees,
			inviteBody.OrgID, inviteBody.InviteeID)
		if err != nil {
			log.Printf("could not find encryption key for invitee: %s",
				inviteBody.InviteeID.String())
			encodeResponseErr(w, err)
			return
		}

		o.Notify(ctx, observer.Progress, "Keyrings retrieved", step, steps)
		step++

		members := []envelope.Signed{}
		for _, segment := range keyringSections {
			krm, err := findKeyringSegmentMember(s.ID(), &segment)
			if err != nil {
				log.Printf("could not find keyring membership: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				enc.Encode(&errorMsg{
					Type:  apitypes.InternalServerError,
					Error: []string{"could not find keyring membership"},
				})
				return
			}

			encPubKey, err := findEncryptionPublicKeyByID(claimTrees, inviteBody.OrgID, krm.EncryptingKeyID)
			if err != nil {
				log.Printf("could not find encypting public key for membership: %s", err)
				encodeResponseErr(w, err)
				return
			}

			encPKBody := encPubKey.Body.(*primitive.PublicKey)
			targetPKBody := targetPubKey.Body.(*primitive.PublicKey)

			encMek, nonce, err := engine.CloneMembership(ctx, *krm.Key.Value,
				*krm.Key.Nonce, &kp.Encryption, *encPKBody.Key.Value, *targetPKBody.Key.Value)
			if err != nil {
				log.Printf("could not clone keyring membership: %s", err)
				encodeResponseErr(w, err)
				return
			}

			member, err := engine.SignedEnvelope(
				ctx, &primitive.KeyringMember{
					Created:         time.Now().UTC(),
					OrgID:           krm.OrgID,
					ProjectID:       krm.ProjectID,
					KeyringID:       krm.KeyringID,
					OwnerID:         inviteBody.InviteeID,
					PublicKeyID:     targetPubKey.ID,
					EncryptingKeyID: encID,

					Key: &primitive.KeyringMemberKey{
						Algorithm: crypto.EasyBox,
						Nonce:     base64.NewValue(nonce),
						Value:     base64.NewValue(encMek),
					},
				},
				sigID, &kp.Signature)
			if err != nil {
				log.Printf("could not create KeyringMember object: %s", err)
				encodeResponseErr(w, err)
				return
			}

			members = append(members, *member)
		}

		o.Notify(ctx, observer.Progress, "Keyring memberships created", step, steps)
		step++

		invite, err = client.OrgInvite.Approve(ctx, &inviteID)
		if err != nil {
			log.Printf("could not approve org invite: %s", err)
			encodeResponseErr(w, err)
			return
		}

		o.Notify(ctx, observer.Progress, "Invite approved", step, steps)
		step++

		if len(members) != 0 {
			_, err = client.KeyringMember.Post(ctx, members)
			if err != nil {
				log.Printf("error uploading memberships: %s", err)
				encodeResponseErr(w, err)
				return
			}
		}

		err = enc.Encode(invite)
		if err != nil {
			log.Printf("error encoding invite approve resp: %s", err)
			encodeResponseErr(w, err)
			return
		}
	}
}

func findKeyringSegmentMember(id *identity.ID,
	section *registry.KeyringSection) (*primitive.KeyringMember, error) {

	var krm *primitive.KeyringMember
	for _, m := range section.Members {
		mBody := m.Body.(*primitive.KeyringMember)
		if *mBody.OwnerID == *id {
			krm = mBody
			break
		}
	}

	if krm == nil {
		err := fmt.Errorf("No keyring membership found for %s", id.String())
		return nil, err
	}

	return krm, nil
}
