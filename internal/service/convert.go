package service

import (
	"time"

	"github.com/mrfroglab/sewain-api/internal/db/dbq"
	"github.com/mrfroglab/sewain-api/internal/domain"
)

// The domain (and the web) use epoch milliseconds on the demo clock and "YYYY-MM-DD" dates in Asia/Jakarta.
// The database uses timestamptz and date.

func tsOf(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func msOf(t time.Time) int64 { return t.UnixMilli() }

func tsPtr(ms *int64) *time.Time {
	if ms == nil {
		return nil
	}
	return domain.Ptr(tsOf(*ms))
}

func msPtr(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	return domain.Ptr(msOf(*t))
}

// dateOf parses a domain date. pgx encodes the calendar date of the time value (UTC), so the date is kept as-is.
func dateOf(iso string) time.Time {
	t, err := time.Parse("2006-01-02", iso)
	if err != nil {
		return time.Time{}
	}
	return t
}

func isoOf(t time.Time) string { return t.UTC().Format("2006-01-02") }

func datePtr(iso *string) *time.Time {
	if iso == nil {
		return nil
	}
	return domain.Ptr(dateOf(*iso))
}

func isoPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	return domain.Ptr(isoOf(*t))
}

func userFromRow(r dbq.User, st *dbq.Store) *domain.User {
	u := &domain.User{
		ID: r.ID, Name: r.Name, Nickname: r.Nickname, Phone: r.Phone, Email: r.Email, Role: r.Role, AccountType: r.AccountType,
		Verification: domain.Verification{Status: r.VerificationStatus, Method: r.VerificationMethod, VerifiedAt: msPtr(r.VerifiedAt), NikMasked: r.NikMasked},
		AreaID:       r.AreaID, Campus: r.Campus, StudentVerified: r.StudentVerified, EarlyLender: r.EarlyLender, Bio: r.Bio,
		JoinedAt: msOf(r.JoinedAt), Strikes: r.Strikes, Status: r.Status, BaseRating: r.BaseRating, BaseRatingCount: r.BaseRatingCount, Demo: r.Demo,
	}
	if st != nil {
		u.Store = &domain.StoreInfo{Name: st.Name, AreaID: st.AreaID, Hours: st.Hours, NIB: st.Nib}
	}
	return u
}

func insertUserParams(u *domain.User) dbq.InsertUserParams {
	return dbq.InsertUserParams{
		ID: u.ID, Phone: u.Phone, Name: u.Name, Nickname: u.Nickname, Email: u.Email, Role: u.Role, AccountType: u.AccountType,
		AreaID: u.AreaID, Campus: u.Campus, Bio: u.Bio, Status: u.Status, Strikes: u.Strikes, VerificationStatus: u.Verification.Status,
		VerificationMethod: u.Verification.Method, VerifiedAt: tsPtr(u.Verification.VerifiedAt), NikMasked: u.Verification.NikMasked,
		EarlyLender: u.EarlyLender, StudentVerified: u.StudentVerified, BaseRating: u.BaseRating, BaseRatingCount: u.BaseRatingCount,
		Demo: u.Demo, JoinedAt: tsOf(u.JoinedAt),
	}
}

func updateUserParams(u *domain.User) dbq.UpdateUserParams {
	return dbq.UpdateUserParams{
		ID: u.ID, Name: u.Name, Nickname: u.Nickname, Email: u.Email, AccountType: u.AccountType, AreaID: u.AreaID, Campus: u.Campus,
		Bio: u.Bio, Status: u.Status, Strikes: u.Strikes, VerificationStatus: u.Verification.Status, VerificationMethod: u.Verification.Method,
		VerifiedAt: tsPtr(u.Verification.VerifiedAt), NikMasked: u.Verification.NikMasked,
	}
}

func verificationFromRow(r dbq.VerificationRequest) *domain.VerificationRequest {
	return &domain.VerificationRequest{
		ID: r.ID, UserID: r.UserID, SubmittedAt: msOf(r.SubmittedAt), Method: r.Method, NameOnKtp: r.NameOnKtp, NikMasked: r.NikMasked,
		KtpPhotoID: r.KtpPhotoID, SelfiePhotoID: r.SelfiePhotoID, Status: r.Status, ReviewedAt: msPtr(r.ReviewedAt), ReviewedBy: r.ReviewedBy,
		Reason: r.Reason, Auto: r.Auto,
	}
}

func listingFromRow(r dbq.ListingRow, photos []string, options []domain.CollateralOption) *domain.Listing {
	l := &domain.Listing{
		ID: r.ID, Slug: r.Slug, OwnerID: r.OwnerID, Title: r.Title, Category: r.Category, Description: r.Description, Photos: photos,
		ConditionNotes: r.ConditionNotes, PricePerDay: r.PricePerDay, PricePerHour: r.PricePerHour, PricePerWeek: r.PricePerWeek,
		DurationDiscount: r.DurationDiscount, MinDays: r.MinDays, MaxDays: r.MaxDays, ItemValue: r.ItemValue, CollateralOptions: options,
		Location: domain.ListingLocation{Lat: r.Lat, Lng: r.Lng, AreaID: r.AreaID}, RadiusKm: r.RadiusKm, Handover: r.Handover,
		MeetingPoint: r.MeetingPoint, HandoverHours: r.HandoverHours,
		Rules:    domain.ListingRules{LateFeePerDay: r.LateFeePerDay, LateFeeCapPercent: r.LateFeeCapPercent, Cancellation: r.Cancellation, Special: r.SpecialRules},
		Approval: r.Approval, AutoApproveMinRating: r.AutoApproveMinRating, Stock: r.Stock, Vehicle: r.Vehicle, Status: r.Status,
		CreatedAt: msOf(r.CreatedAt), BaseRating: r.BaseRating, BaseRatingCount: r.BaseRatingCount, RentCount: r.RentCount,
	}
	domain.NormListing(l)
	return l
}

func collateralFromRow(r dbq.CollateralOption) domain.CollateralOption {
	return domain.CollateralOption{ID: r.ID, Kind: r.Kind, Cards: r.Cards, DepositMode: r.DepositMode, DepositAmount: r.DepositAmount,
		DepositPercent: r.DepositPercent, WithIdentity: r.WithIdentity, Description: r.Description, EstimatedValue: r.EstimatedValue}
}

func insertListingParams(l *domain.Listing) dbq.InsertListingParams {
	return dbq.InsertListingParams{
		ID: l.ID, Slug: l.Slug, OwnerID: l.OwnerID, Title: l.Title, Category: l.Category, Description: l.Description,
		ConditionNotes: l.ConditionNotes, PricePerDay: l.PricePerDay, PricePerHour: l.PricePerHour, PricePerWeek: l.PricePerWeek,
		DurationDiscount: l.DurationDiscount, MinDays: l.MinDays, MaxDays: l.MaxDays, ItemValue: l.ItemValue, Lat: l.Location.Lat,
		Lng: l.Location.Lng, AreaID: l.Location.AreaID, RadiusKm: l.RadiusKm, Handover: l.Handover, MeetingPoint: l.MeetingPoint,
		HandoverHours: l.HandoverHours, LateFeePerDay: l.Rules.LateFeePerDay, LateFeeCapPercent: l.Rules.LateFeeCapPercent,
		Cancellation: l.Rules.Cancellation, SpecialRules: l.Rules.Special, Approval: l.Approval, AutoApproveMinRating: l.AutoApproveMinRating,
		Stock: l.Stock, Vehicle: l.Vehicle, Status: l.Status, RentCount: l.RentCount, BaseRating: l.BaseRating,
		BaseRatingCount: l.BaseRatingCount, CreatedAt: tsOf(l.CreatedAt),
	}
}

func updateListingParams(l *domain.Listing) dbq.UpdateListingParams {
	p := insertListingParams(l)
	return dbq.UpdateListingParams{
		ID: p.ID, Title: p.Title, Category: p.Category, Description: p.Description, ConditionNotes: p.ConditionNotes,
		PricePerDay: p.PricePerDay, PricePerHour: p.PricePerHour, PricePerWeek: p.PricePerWeek, DurationDiscount: p.DurationDiscount,
		MinDays: p.MinDays, MaxDays: p.MaxDays, ItemValue: p.ItemValue, Lat: p.Lat, Lng: p.Lng, AreaID: p.AreaID, RadiusKm: p.RadiusKm,
		Handover: p.Handover, MeetingPoint: p.MeetingPoint, HandoverHours: p.HandoverHours, LateFeePerDay: p.LateFeePerDay,
		LateFeeCapPercent: p.LateFeeCapPercent, Cancellation: p.Cancellation, SpecialRules: p.SpecialRules, Approval: p.Approval,
		AutoApproveMinRating: p.AutoApproveMinRating, Stock: p.Stock, Vehicle: p.Vehicle, Status: p.Status, RentCount: p.RentCount,
	}
}

func bookingFromRow(r dbq.Booking) *domain.Booking {
	ag := r.Agreement
	ag.RenterAgreedAt, ag.ProviderAgreedAt = msPtr(r.RenterAgreedAt), msPtr(r.ProviderAgreedAt)
	if ag.ReferencePhotos == nil {
		ag.ReferencePhotos = []string{}
	}
	return &domain.Booking{
		ID: r.ID, Code: r.Code, ListingID: r.ListingID, RenterID: r.RenterID, ProviderID: r.ProviderID, StartDate: isoOf(r.StartDate),
		EndDate: isoOf(r.EndDate), Handover: r.Handover, Collateral: r.Collateral, Note: r.Note, Status: r.Status, Overdue: r.Overdue,
		CreatedAt: msOf(r.CreatedAt), ExpiresAt: msOf(r.ExpiresAt), AcceptedAt: msPtr(r.AcceptedAt), DeclinedAt: msPtr(r.DeclinedAt),
		DeclineReason: r.DeclineReason, CancelledAt: msPtr(r.CancelledAt), CancelledBy: r.CancelledBy, CancelReason: r.CancelReason,
		ActiveAt: msPtr(r.ActiveAt), ReturnedAt: msPtr(r.ReturnedAt), CompletedAt: msPtr(r.CompletedAt), CompletedAuto: r.CompletedAuto,
		ResolvedAt: msPtr(r.ResolvedAt), Agreement: ag, Payment: r.Payment, MeetingPointDetail: r.MeetingPointDetail, Checkout: r.Checkout,
		Checkin: r.Checkin, CollateralReturn: r.CollateralReturn, DisputeID: r.DisputeID,
	}
}

// storedAgreement strips the agreement timestamps (kept in their own columns).
func storedAgreement(ag domain.Agreement) domain.Agreement {
	ag.RenterAgreedAt, ag.ProviderAgreedAt = nil, nil
	return ag
}

func insertBookingParams(b *domain.Booking) dbq.InsertBookingParams {
	c := b.Agreement.Commission
	return dbq.InsertBookingParams{
		ID: b.ID, Code: b.Code, ListingID: b.ListingID, RenterID: b.RenterID, ProviderID: b.ProviderID, StartDate: dateOf(b.StartDate),
		EndDate: dateOf(b.EndDate), Handover: b.Handover, Collateral: b.Collateral, Note: b.Note, Status: b.Status, Overdue: b.Overdue,
		CreatedAt: tsOf(b.CreatedAt), ExpiresAt: tsOf(b.ExpiresAt), AcceptedAt: tsPtr(b.AcceptedAt), DeclinedAt: tsPtr(b.DeclinedAt),
		DeclineReason: b.DeclineReason, CancelledAt: tsPtr(b.CancelledAt), CancelledBy: b.CancelledBy, CancelReason: b.CancelReason,
		ActiveAt: tsPtr(b.ActiveAt), ReturnedAt: tsPtr(b.ReturnedAt), CompletedAt: tsPtr(b.CompletedAt), CompletedAuto: b.CompletedAuto,
		ResolvedAt: tsPtr(b.ResolvedAt), Agreement: storedAgreement(b.Agreement), AgreementHash: b.Agreement.Hash,
		RenterAgreedAt: tsPtr(b.Agreement.RenterAgreedAt), ProviderAgreedAt: tsPtr(b.Agreement.ProviderAgreedAt),
		RentTotal: b.Agreement.Quote.Total, CollateralValue: b.Agreement.CollateralValue, CommissionAmount: c.Amount,
		CommissionCharged: c.Charged, CommissionBearer: c.Bearer, Payment: b.Payment, MeetingPointDetail: b.MeetingPointDetail,
		Checkout: b.Checkout, Checkin: b.Checkin, CollateralReturn: b.CollateralReturn, DisputeID: b.DisputeID,
	}
}

func updateBookingParams(b *domain.Booking) dbq.UpdateBookingParams {
	return dbq.UpdateBookingParams{
		ID: b.ID, Status: b.Status, Overdue: b.Overdue, AcceptedAt: tsPtr(b.AcceptedAt), DeclinedAt: tsPtr(b.DeclinedAt),
		DeclineReason: b.DeclineReason, CancelledAt: tsPtr(b.CancelledAt), CancelledBy: b.CancelledBy, CancelReason: b.CancelReason,
		ActiveAt: tsPtr(b.ActiveAt), ReturnedAt: tsPtr(b.ReturnedAt), CompletedAt: tsPtr(b.CompletedAt), CompletedAuto: b.CompletedAuto,
		ResolvedAt: tsPtr(b.ResolvedAt), RenterAgreedAt: tsPtr(b.Agreement.RenterAgreedAt), ProviderAgreedAt: tsPtr(b.Agreement.ProviderAgreedAt),
		Payment: b.Payment, MeetingPointDetail: b.MeetingPointDetail, Checkout: b.Checkout, Checkin: b.Checkin,
		CollateralReturn: b.CollateralReturn, DisputeID: b.DisputeID,
	}
}

func photoFromRow(r dbq.Photo) *domain.PhotoRecord {
	return &domain.PhotoRecord{ID: r.ID, BookingID: r.BookingID, Stage: r.Stage, UploaderID: r.UploaderID, Src: r.Src, Sha256: r.Sha256,
		ServerTimestamp: msOf(r.ServerTs), Source: r.Source, Angle: r.Angle, Width: r.Width, Height: r.Height, Bytes: r.Bytes}
}

// SystemSender is the senderId of system messages in the web app (NULL in the database).
const SystemSender = "system"

func messageFromRow(r dbq.Message) *domain.Message {
	sender := SystemSender
	if r.SenderID != nil {
		sender = *r.SenderID
	}
	return &domain.Message{ID: r.ID, BookingID: r.BookingID, SenderID: sender, Text: r.Text, PhotoID: r.PhotoID, At: msOf(r.At)}
}

func reviewFromRow(r dbq.Review) *domain.Review {
	return &domain.Review{ID: r.ID, BookingID: r.BookingID, ListingID: r.ListingID, FromID: r.FromID, ToID: r.ToID, FromRole: r.FromRole,
		Rating: r.Rating, Comment: r.Comment, CreatedAt: msOf(r.CreatedAt)}
}

func reportFromRow(r dbq.Report) *domain.Report {
	return &domain.Report{ID: r.ID, ReporterID: r.ReporterID, TargetType: r.TargetType, TargetID: r.TargetID, Reason: r.Reason,
		Detail: r.Detail, CreatedAt: msOf(r.CreatedAt), Status: r.Status, Resolution: r.Resolution, ResolvedAt: msPtr(r.ResolvedAt),
		ResolvedBy: r.ResolvedBy}
}

func disputeFromRow(r dbq.Dispute, proposals []domain.DisputeProposal, photos []domain.ID) *domain.Dispute {
	if proposals == nil {
		proposals = []domain.DisputeProposal{}
	}
	if photos == nil {
		photos = []domain.ID{}
	}
	return &domain.Dispute{ID: r.ID, BookingID: r.BookingID, OpenedBy: r.OpenedBy, CaseType: r.CaseType, Description: r.Description,
		PhotoIDs: photos, CreatedAt: msOf(r.CreatedAt), Layer: r.Layer, Status: r.Status, L1Deadline: msOf(r.L1Deadline),
		Proposals: proposals, Recommendation: r.Recommendation, Outcome: r.Outcome, ResolvedAt: msPtr(r.ResolvedAt)}
}

func proposalFromRow(r dbq.DisputeProposal) domain.DisputeProposal {
	return domain.DisputeProposal{ID: r.ID, ByID: r.ByID, Type: r.Type, Amount: r.Amount, Note: r.Note, At: msOf(r.At), Status: r.Status}
}

func strikeFromRow(r dbq.Strike) *domain.Strike {
	return &domain.Strike{ID: r.ID, UserID: r.UserID, Reason: r.Reason, Severity: r.Severity, BookingID: r.BookingID, At: msOf(r.At), ByAdminID: r.ByAdminID}
}

func auditFromRow(r dbq.AuditLog) *domain.AuditEntry {
	return &domain.AuditEntry{ID: r.ID, ActorID: r.ActorID, Action: r.Action, Target: r.Target, OldValue: r.OldValue, NewValue: r.NewValue, At: msOf(r.At)}
}

func notificationFromRow(r dbq.Notification) *domain.Notification {
	return &domain.Notification{ID: r.ID, UserID: r.UserID, Kind: r.Kind, Title: r.Title, Body: r.Body, Href: r.Href, At: msOf(r.At), Read: r.ReadAt != nil}
}

// wantedRow has the shape of the sqlc rows of the wanted_posts queries (convertible with wantedRow(r)).
type wantedRow struct {
	ID           string
	Ord          int64
	RequesterID  string
	Title        string
	Description  string
	Category     *string
	AreaID       string
	Lat, Lng     float64
	RadiusKm     float64
	NeededFrom   *time.Time
	NeededUntil  *time.Time
	BudgetPerDay *int64
	Status       string
	CreatedAt    time.Time
}

func wantedFromRow(r wantedRow, offers []domain.WantedOffer) *domain.WantedPost {
	if offers == nil {
		offers = []domain.WantedOffer{}
	}
	return &domain.WantedPost{ID: r.ID, RequesterID: r.RequesterID, Title: r.Title, Description: r.Description, Category: r.Category,
		AreaID: r.AreaID, Location: domain.GeoPoint{Lat: r.Lat, Lng: r.Lng}, RadiusKm: r.RadiusKm, NeededFrom: isoPtr(r.NeededFrom),
		NeededUntil: isoPtr(r.NeededUntil), BudgetPerDay: r.BudgetPerDay, CreatedAt: msOf(r.CreatedAt), Status: r.Status, Offers: offers}
}

func offerFromRow(r dbq.WantedOffer) domain.WantedOffer {
	return domain.WantedOffer{ID: r.ID, ProviderID: r.ProviderID, ListingID: r.ListingID, Message: r.Message, At: msOf(r.At)}
}

func commissionFromRow(r dbq.CommissionConfig) domain.CommissionConfig {
	return domain.CommissionConfig{Active: r.Active, Mode: r.Mode, Percent: r.Percent, Flat: r.Flat, Min: r.Min, Max: r.Max, Bearer: r.Bearer,
		SplitRenterPercent: r.SplitRenterPercent, UpdatedAt: msOf(r.EffectiveFrom), UpdatedBy: r.CreatedBy}
}

func rulesFromRow(r dbq.CollateralRuleSet) domain.CollateralRules {
	return domain.CollateralRules{LowMax: r.LowMax, MidMax: r.MidMax, MidDepositPercent: r.MidDepositPercent, HighDepositPercent: r.HighDepositPercent,
		VehicleDepositPercent: r.VehicleDepositPercent, HighValueBadge: r.HighValueBadge, UpdatedAt: msOf(r.EffectiveFrom), UpdatedBy: r.CreatedBy}
}
