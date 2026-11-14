package domain

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrForbidden: the token's user may not act as the actor in the arguments.
var ErrForbidden = errors.New("forbidden")

// ErrUnknownAction: not a server-side action.
var ErrUnknownAction = errors.New("unknown action")

// ClientOnly actions exist in the web store but have no server effect.
var ClientOnly = map[string]bool{"setSearch": true, "addPhotos": true, "tick": true, "now": true}

// Dispatch runs a web-store action on the server. args are the exact positional arguments the web app passed;
// wantID is the id the client generated for a newly created entity. The caller holds the engine lock.
func (e *Engine) Dispatch(viewer *User, name string, args []json.RawMessage, wantID string) (any, error) {
	arg := func(i int, v any) error {
		if i >= len(args) || string(args[i]) == "null" {
			return nil
		}
		if err := json.Unmarshal(args[i], v); err != nil {
			return fail(fmt.Sprintf("Argumen %d tidak valid.", i+1))
		}
		return nil
	}
	str := func(i int) string {
		var s string
		_ = arg(i, &s)
		return s
	}
	// The first argument of almost every action is the acting user: it must be the signed-in user.
	actor := func() (ID, error) {
		a := str(0)
		if viewer == nil || a != viewer.ID {
			return "", ErrForbidden
		}
		return a, nil
	}
	var none any

	switch name {
	case "registerUser":
		var in RegisterInput
		if err := arg(0, &in); err != nil {
			return nil, err
		}
		return e.RegisterUser(in, wantID)
	}

	a, err := actor()
	if err != nil {
		return nil, err
	}
	switch name {
	case "updateUser":
		var p UserPatch
		if err := arg(1, &p); err != nil {
			return nil, err
		}
		return none, e.UpdateUser(a, p)
	case "submitVerification":
		var in VerificationInput
		if err := arg(1, &in); err != nil {
			return nil, err
		}
		return e.SubmitVerification(a, in, wantID)
	case "reviewVerification":
		var approve bool
		var reason *string
		_ = arg(2, &approve)
		_ = arg(3, &reason)
		return none, e.ReviewVerification(a, str(1), approve, reason)
	case "createListing":
		var in ListingInput
		if err := arg(1, &in); err != nil {
			return nil, err
		}
		return e.CreateListing(a, in, wantID)
	case "updateListing":
		var in ListingInput
		if err := arg(2, &in); err != nil {
			return nil, err
		}
		return none, e.UpdateListing(a, str(1), in)
	case "setListingStatus":
		return none, e.SetListingStatus(a, str(1), str(2))
	case "toggleFavorite":
		return none, e.ToggleFavorite(a, str(1))
	case "createBooking":
		var in BookingInput
		if err := arg(1, &in); err != nil {
			return nil, err
		}
		return e.CreateBooking(a, in, wantID)
	case "acceptBooking":
		var meeting *string
		_ = arg(2, &meeting)
		return none, e.AcceptBooking(a, str(1), meeting)
	case "declineBooking":
		return none, e.DeclineBooking(a, str(1), str(2))
	case "cancelBooking":
		return none, e.CancelBooking(a, str(1), str(2))
	case "payBooking":
		return none, e.PayBooking(a, str(1), str(2))
	case "submitCheckout":
		var in CheckoutInput
		if err := arg(2, &in); err != nil {
			return nil, err
		}
		return none, e.SubmitCheckout(a, str(1), in)
	case "addCheckoutPhotos":
		var ids []ID
		_ = arg(2, &ids)
		return none, e.AddCheckoutPhotos(a, str(1), ids)
	case "agreeCheckout":
		return none, e.AgreeCheckout(a, str(1))
	case "rejectAtCheckout":
		return none, e.RejectAtCheckout(a, str(1), str(2))
	case "submitCheckin":
		var in CheckinInput
		if err := arg(2, &in); err != nil {
			return nil, err
		}
		return none, e.SubmitCheckin(a, str(1), in)
	case "returnCollateral":
		return none, e.ReturnCollateral(a, str(1))
	case "confirmCollateral":
		return none, e.ConfirmCollateral(a, str(1))
	case "submitReview":
		var rating float64
		_ = arg(2, &rating)
		return none, e.SubmitReview(a, str(1), rating, str(3))
	case "sendMessage":
		var in struct {
			Text    *string `json:"text"`
			PhotoID *ID     `json:"photoId"`
		}
		_ = arg(2, &in)
		return none, e.SendMessage(a, str(1), in.Text, in.PhotoID)
	case "markThreadRead":
		return none, e.MarkThreadRead(a, str(1))
	case "markNotificationsRead":
		var ids []ID
		_ = arg(1, &ids)
		return none, e.MarkNotificationsRead(a, ids)
	case "openDispute":
		var in DisputeInput
		if err := arg(2, &in); err != nil {
			return nil, err
		}
		return e.OpenDispute(a, str(1), in, wantID)
	case "proposeSolution":
		var in ProposalInput
		if err := arg(2, &in); err != nil {
			return nil, err
		}
		return none, e.ProposeSolution(a, str(1), in)
	case "respondProposal":
		var accept bool
		_ = arg(3, &accept)
		return none, e.RespondProposal(a, str(1), str(2), accept)
	case "escalateDispute":
		return none, e.EscalateDispute(a, str(1))
	case "adminRecommend":
		var in struct {
			Text   string `json:"text"`
			Amount *int64 `json:"amount"`
		}
		_ = arg(2, &in)
		return none, e.AdminRecommend(a, str(1), in.Text, in.Amount)
	case "resolveDispute":
		return none, e.ResolveDispute(a, str(1), str(2))
	case "createReport":
		var in ReportInput
		if err := arg(1, &in); err != nil {
			return nil, err
		}
		return e.CreateReport(a, in, wantID)
	case "resolveReport":
		return none, e.ResolveReport(a, str(1), str(2), str(3))
	case "addStrike":
		var in StrikeInput
		_ = arg(2, &in)
		return none, e.AddStrike(a, str(1), in)
	case "setUserStatus":
		return none, e.SetUserStatus(a, str(1), str(2), str(3))
	case "updateCommission":
		var p CommissionPatch
		if err := arg(1, &p); err != nil {
			return nil, err
		}
		var keys map[string]json.RawMessage
		_ = arg(1, &keys)
		_, p.HasMin = keys["min"]
		_, p.HasMax = keys["max"]
		return none, e.UpdateCommission(a, p)
	case "updateCollateralRules":
		var p CollateralRulesPatch
		if err := arg(1, &p); err != nil {
			return nil, err
		}
		return none, e.UpdateCollateralRules(a, p)
	case "setBannedKeywords":
		var words []string
		_ = arg(1, &words)
		return none, e.SetBannedKeywords(a, words)
	case "createWanted":
		var in WantedInput
		if err := arg(1, &in); err != nil {
			return nil, err
		}
		return e.CreateWanted(a, in, wantID)
	case "offerWanted":
		var in struct {
			ListingID *ID    `json:"listingId"`
			Message   string `json:"message"`
		}
		_ = arg(2, &in)
		return none, e.OfferWanted(a, str(1), in.ListingID, in.Message)
	case "closeWanted":
		return none, e.CloseWanted(a, str(1), str(2))
	}
	return nil, ErrUnknownAction
}
