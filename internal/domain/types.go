// Package domain holds Sewain's business rules: accounts & verification, listings, the booking state machine,
// collateral, commission, photo evidence, disputes (L1–L3), moderation and the demo clock.
// Types mirror sewain-web/src/lib/types.ts one-to-one so the JSON is shared by both sides.
// Timestamps are epoch milliseconds on the demo clock; date-only values are "YYYY-MM-DD" (Asia/Jakarta).
package domain

type ID = string

type StoreInfo struct {
	Name   string  `json:"name"`
	AreaID string  `json:"areaId"`
	Hours  string  `json:"hours"`
	NIB    *string `json:"nib,omitempty"`
}

type Verification struct {
	Status     string  `json:"status"` // belum | menunggu | terverifikasi | ditolak
	Method     *string `json:"method,omitempty"`
	VerifiedAt *int64  `json:"verifiedAt,omitempty"`
	NikMasked  *string `json:"nikMasked,omitempty"`
}

type DemoInfo struct {
	Label   string `json:"label"`
	Persona string `json:"persona"`
}

type User struct {
	ID              ID           `json:"id"`
	Name            string       `json:"name"`
	Nickname        *string      `json:"nickname,omitempty"`
	Phone           string       `json:"phone"`
	Email           *string      `json:"email,omitempty"`
	Role            string       `json:"role"`        // user | admin
	AccountType     string       `json:"accountType"` // individu | toko
	Store           *StoreInfo   `json:"store,omitempty"`
	Verification    Verification `json:"verification"`
	AreaID          string       `json:"areaId"`
	Campus          *string      `json:"campus,omitempty"`
	StudentVerified *bool        `json:"studentVerified,omitempty"`
	EarlyLender     *bool        `json:"earlyLender,omitempty"`
	Bio             *string      `json:"bio,omitempty"`
	JoinedAt        int64        `json:"joinedAt"`
	Strikes         int          `json:"strikes"`
	Status          string       `json:"status"` // aktif | suspend | blacklist
	BaseRating      *float64     `json:"baseRating,omitempty"`
	BaseRatingCount *int         `json:"baseRatingCount,omitempty"`
	Demo            *DemoInfo    `json:"demo,omitempty"`
	// PublicStats is computed by the server for every profile in a snapshot (web-v2 needs #6).
	PublicStats *PublicStats `json:"publicStats,omitempty"`
}

// PublicStats are the public reputation numbers of a user, computed on the server because a scoped snapshot does
// not carry other people's bookings, disputes or strikes.
type PublicStats struct {
	CompletedRentals int `json:"completedRentals"` // COMPLETED bookings as renter or provider + baseRatingCount
	Disputes         int `json:"disputes"`         // disputes on bookings the user is a party of
	TrustScore       int `json:"trustScore"`       // 0–100, TrustScore
	Strikes          int `json:"strikes"`          // public (PRD §11.2 L3 "pernah bersengketa" badge)
}

type VerificationRequest struct {
	ID            ID      `json:"id"`
	UserID        ID      `json:"userId"`
	SubmittedAt   int64   `json:"submittedAt"`
	Method        string  `json:"method"`
	NameOnKtp     string  `json:"nameOnKtp"`
	NikMasked     string  `json:"nikMasked"`
	KtpPhotoID    *ID     `json:"ktpPhotoId,omitempty"`
	SelfiePhotoID *ID     `json:"selfiePhotoId,omitempty"`
	Status        string  `json:"status"` // menunggu | disetujui | ditolak
	ReviewedAt    *int64  `json:"reviewedAt,omitempty"`
	ReviewedBy    *ID     `json:"reviewedBy,omitempty"`
	Reason        *string `json:"reason,omitempty"`
	Auto          bool    `json:"auto"`
}

type GeoPoint struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

type ListingLocation struct {
	Lat    float64 `json:"lat"`
	Lng    float64 `json:"lng"`
	AreaID string  `json:"areaId"`
}

type CollateralOption struct {
	ID             ID       `json:"id"`
	Kind           string   `json:"kind"` // identitas | deposit | barang | custom
	Cards          []string `json:"cards,omitempty"`
	DepositMode    *string  `json:"depositMode,omitempty"` // tetap | persen
	DepositAmount  *int64   `json:"depositAmount,omitempty"`
	DepositPercent *float64 `json:"depositPercent,omitempty"`
	WithIdentity   *bool    `json:"withIdentity,omitempty"`
	Description    *string  `json:"description,omitempty"`
	EstimatedValue *int64   `json:"estimatedValue,omitempty"`
}

type ListingRules struct {
	LateFeePerDay     int64   `json:"lateFeePerDay"`
	LateFeeCapPercent float64 `json:"lateFeeCapPercent"`
	Cancellation      string  `json:"cancellation"`
	Special           *string `json:"special,omitempty"`
}

type VehicleInfo struct {
	PlateMasked    string `json:"plateMasked"`
	StnkValidUntil string `json:"stnkValidUntil"`
	SimClass       string `json:"simClass"`
}

type DurationDiscount struct {
	MinDays int     `json:"minDays"`
	Percent float64 `json:"percent"`
}

type Listing struct {
	ID                   ID                 `json:"id"`
	Slug                 string             `json:"slug"`
	OwnerID              ID                 `json:"ownerId"`
	Title                string             `json:"title"`
	Category             string             `json:"category"`
	Description          string             `json:"description"`
	Photos               []string           `json:"photos"`
	ConditionNotes       string             `json:"conditionNotes"`
	PricePerDay          int64              `json:"pricePerDay"`
	PricePerHour         *int64             `json:"pricePerHour,omitempty"`
	PricePerWeek         *int64             `json:"pricePerWeek,omitempty"`
	DurationDiscount     *DurationDiscount  `json:"durationDiscount,omitempty"`
	MinDays              *int               `json:"minDays,omitempty"`
	MaxDays              *int               `json:"maxDays,omitempty"`
	ItemValue            int64              `json:"itemValue"`
	CollateralOptions    []CollateralOption `json:"collateralOptions"`
	Location             ListingLocation    `json:"location"`
	RadiusKm             float64            `json:"radiusKm"`
	Handover             []string           `json:"handover"`
	MeetingPoint         *string            `json:"meetingPoint,omitempty"`
	HandoverHours        *string            `json:"handoverHours,omitempty"`
	Rules                ListingRules       `json:"rules"`
	Approval             string             `json:"approval"` // manual | otomatis
	AutoApproveMinRating *float64           `json:"autoApproveMinRating,omitempty"`
	Stock                int                `json:"stock"`
	Vehicle              *VehicleInfo       `json:"vehicle,omitempty"`
	Status               string             `json:"status"` // aktif | jeda | diturunkan
	CreatedAt            int64              `json:"createdAt"`
	BaseRating           float64            `json:"baseRating"`
	BaseRatingCount      int                `json:"baseRatingCount"`
	RentCount            int                `json:"rentCount"`
}

type RentQuote struct {
	Days      int    `json:"days"`
	Base      int64  `json:"base"`
	Discount  int64  `json:"discount"`
	Total     int64  `json:"total"`
	Breakdown string `json:"breakdown"`
}

type CommissionConfig struct {
	Active             bool    `json:"active"`
	Mode               string  `json:"mode"` // persen | flat
	Percent            float64 `json:"percent"`
	Flat               int64   `json:"flat"`
	Min                *int64  `json:"min,omitempty"`
	Max                *int64  `json:"max,omitempty"`
	Bearer             string  `json:"bearer"` // penyewa | penyedia | split
	SplitRenterPercent float64 `json:"splitRenterPercent"`
	UpdatedAt          int64   `json:"updatedAt"`
	UpdatedBy          ID      `json:"updatedBy"`
}

type CommissionSnapshot struct {
	Active             bool    `json:"active"`
	Mode               string  `json:"mode"`
	Rate               float64 `json:"rate"`
	Min                *int64  `json:"min,omitempty"`
	Max                *int64  `json:"max,omitempty"`
	Bearer             string  `json:"bearer"`
	SplitRenterPercent float64 `json:"splitRenterPercent"`
	Amount             int64   `json:"amount"`
	Charged            int64   `json:"charged"`
	RenterPays         int64   `json:"renterPays"`
	ProviderPays       int64   `json:"providerPays"`
}

type Agreement struct {
	CreatedAt        int64              `json:"createdAt"`
	ListingID        ID                 `json:"listingId"`
	ListingTitle     string             `json:"listingTitle"`
	Category         string             `json:"category"`
	ItemValue        int64              `json:"itemValue"`
	RenterName       string             `json:"renterName"`
	ProviderName     string             `json:"providerName"`
	StartDate        string             `json:"startDate"`
	EndDate          string             `json:"endDate"`
	Quote            RentQuote          `json:"quote"`
	Collateral       CollateralOption   `json:"collateral"`
	CollateralValue  int64              `json:"collateralValue"`
	Handover         string             `json:"handover"`
	LateFeePerDay    int64              `json:"lateFeePerDay"`
	LateFeeCap       int64              `json:"lateFeeCap"`
	Cancellation     string             `json:"cancellation"`
	Special          *string            `json:"special,omitempty"`
	ConditionNotes   string             `json:"conditionNotes"`
	ReferencePhotos  []string           `json:"referencePhotos"`
	Commission       CommissionSnapshot `json:"commission"`
	Hash             string             `json:"hash"`
	RenterAgreedAt   *int64             `json:"renterAgreedAt,omitempty"`
	ProviderAgreedAt *int64             `json:"providerAgreedAt,omitempty"`
}

type PaymentInfo struct {
	Status string  `json:"status"`           // belum | lunas
	Method *string `json:"method,omitempty"` // qris | va | ewallet | tunai (simulated) | langsung (offline)
	PaidAt *int64  `json:"paidAt,omitempty"`
	// ConfirmedAt: the provider confirmed receiving a direct ("langsung") payment (PAYMENT_MODE=offline).
	ConfirmedAt *int64 `json:"confirmedAt,omitempty"`
	Rent        int64  `json:"rent"`
	Deposit     int64  `json:"deposit"`
	Commission  int64  `json:"commission"`
	Total       int64  `json:"total"`
}

type Checkout struct {
	PhotoIDs           []ID    `json:"photoIds"`
	CollateralPhotoIDs []ID    `json:"collateralPhotoIds"`
	Odometer           *string `json:"odometer,omitempty"`
	Fuel               *string `json:"fuel,omitempty"`
	SubmittedAt        int64   `json:"submittedAt"`
	RenterAgreedAt     *int64  `json:"renterAgreedAt,omitempty"`
	ProviderAgreedAt   *int64  `json:"providerAgreedAt,omitempty"`
}

type Checkin struct {
	PhotoIDs    []ID    `json:"photoIds"`
	Odometer    *string `json:"odometer,omitempty"`
	Fuel        *string `json:"fuel,omitempty"`
	Result      string  `json:"result"` // sesuai | masalah
	ProblemType *string `json:"problemType,omitempty"`
	ProblemNote *string `json:"problemNote,omitempty"`
	At          int64   `json:"at"`
}

type CollateralReturn struct {
	ProviderReturnedAt *int64 `json:"providerReturnedAt,omitempty"`
	RenterConfirmedAt  *int64 `json:"renterConfirmedAt,omitempty"`
	AutoCompleteAt     *int64 `json:"autoCompleteAt,omitempty"`
}

type Booking struct {
	ID                 ID                `json:"id"`
	Code               string            `json:"code"`
	ListingID          ID                `json:"listingId"`
	RenterID           ID                `json:"renterId"`
	ProviderID         ID                `json:"providerId"`
	StartDate          string            `json:"startDate"`
	EndDate            string            `json:"endDate"`
	Handover           string            `json:"handover"`
	Collateral         CollateralOption  `json:"collateral"`
	Note               *string           `json:"note,omitempty"`
	Status             string            `json:"status"`
	Overdue            bool              `json:"overdue"`
	CreatedAt          int64             `json:"createdAt"`
	ExpiresAt          int64             `json:"expiresAt"`
	AcceptedAt         *int64            `json:"acceptedAt,omitempty"`
	DeclinedAt         *int64            `json:"declinedAt,omitempty"`
	DeclineReason      *string           `json:"declineReason,omitempty"`
	CancelledAt        *int64            `json:"cancelledAt,omitempty"`
	CancelledBy        *ID               `json:"cancelledBy,omitempty"`
	CancelReason       *string           `json:"cancelReason,omitempty"`
	ActiveAt           *int64            `json:"activeAt,omitempty"`
	ReturnedAt         *int64            `json:"returnedAt,omitempty"`
	CompletedAt        *int64            `json:"completedAt,omitempty"`
	CompletedAuto      *bool             `json:"completedAuto,omitempty"`
	ResolvedAt         *int64            `json:"resolvedAt,omitempty"`
	Agreement          Agreement         `json:"agreement"`
	Payment            PaymentInfo       `json:"payment"`
	MeetingPointDetail *string           `json:"meetingPointDetail,omitempty"`
	Checkout           *Checkout         `json:"checkout,omitempty"`
	Checkin            *Checkin          `json:"checkin,omitempty"`
	CollateralReturn   *CollateralReturn `json:"collateralReturn,omitempty"`
	DisputeID          *ID               `json:"disputeId,omitempty"`
}

type PhotoRecord struct {
	ID              ID      `json:"id"`
	BookingID       *ID     `json:"bookingId,omitempty"`
	Stage           string  `json:"stage"`
	UploaderID      ID      `json:"uploaderId"`
	Src             string  `json:"src"`
	Sha256          string  `json:"sha256"`
	ServerTimestamp int64   `json:"serverTimestamp"`
	Source          string  `json:"source"` // kamera | galeri | demo
	Angle           *string `json:"angle,omitempty"`
	Width           *int    `json:"width,omitempty"`
	Height          *int    `json:"height,omitempty"`
	Bytes           *int64  `json:"bytes,omitempty"`
}

type Message struct {
	ID        ID      `json:"id"`
	BookingID ID      `json:"bookingId"`
	SenderID  ID      `json:"senderId"` // user id or "system"
	Text      *string `json:"text,omitempty"`
	PhotoID   *ID     `json:"photoId,omitempty"`
	At        int64   `json:"at"`
}

type Review struct {
	ID        ID      `json:"id"`
	BookingID ID      `json:"bookingId"`
	ListingID ID      `json:"listingId"`
	FromID    ID      `json:"fromId"`
	ToID      ID      `json:"toId"`
	FromRole  string  `json:"fromRole"` // penyewa | penyedia
	Rating    float64 `json:"rating"`
	Comment   string  `json:"comment"`
	CreatedAt int64   `json:"createdAt"`
	// VisibleAt is when the review is (or becomes) visible to others (FR-T2): both parties reviewed, or +7 days.
	VisibleAt *int64 `json:"visibleAt,omitempty"`
}

type Report struct {
	ID         ID      `json:"id"`
	ReporterID ID      `json:"reporterId"`
	TargetType string  `json:"targetType"` // listing | user
	TargetID   ID      `json:"targetId"`
	Reason     string  `json:"reason"`
	Detail     *string `json:"detail,omitempty"`
	CreatedAt  int64   `json:"createdAt"`
	Status     string  `json:"status"` // baru | ditindak | ditolak
	Resolution *string `json:"resolution,omitempty"`
	ResolvedAt *int64  `json:"resolvedAt,omitempty"`
	ResolvedBy *ID     `json:"resolvedBy,omitempty"`
}

type DisputeProposal struct {
	ID     ID      `json:"id"`
	ByID   ID      `json:"byId"`
	Type   string  `json:"type"`
	Amount *int64  `json:"amount,omitempty"`
	Note   *string `json:"note,omitempty"`
	At     int64   `json:"at"`
	Status string  `json:"status"` // diajukan | diterima | ditolak
}

type Recommendation struct {
	Text      string `json:"text"`
	Amount    *int64 `json:"amount,omitempty"`
	ByAdminID ID     `json:"byAdminId"`
	At        int64  `json:"at"`
}

type Dispute struct {
	ID             ID                `json:"id"`
	BookingID      ID                `json:"bookingId"`
	OpenedBy       ID                `json:"openedBy"`
	CaseType       string            `json:"caseType"`
	Description    string            `json:"description"`
	PhotoIDs       []ID              `json:"photoIds"`
	CreatedAt      int64             `json:"createdAt"`
	Layer          string            `json:"layer"`  // L1 | L2 | L3
	Status         string            `json:"status"` // negosiasi | mediasi | selesai
	L1Deadline     int64             `json:"l1Deadline"`
	Proposals      []DisputeProposal `json:"proposals"`
	Recommendation *Recommendation   `json:"recommendation,omitempty"`
	Outcome        *string           `json:"outcome,omitempty"`
	ResolvedAt     *int64            `json:"resolvedAt,omitempty"`
}

type Strike struct {
	ID        ID     `json:"id"`
	UserID    ID     `json:"userId"`
	Reason    string `json:"reason"`
	Severity  string `json:"severity"` // ringan | berat
	BookingID *ID    `json:"bookingId,omitempty"`
	At        int64  `json:"at"`
	ByAdminID ID     `json:"byAdminId"`
}

type CollateralRules struct {
	LowMax                int64   `json:"lowMax"`
	MidMax                int64   `json:"midMax"`
	MidDepositPercent     float64 `json:"midDepositPercent"`
	HighDepositPercent    float64 `json:"highDepositPercent"`
	VehicleDepositPercent float64 `json:"vehicleDepositPercent"`
	HighValueBadge        int64   `json:"highValueBadge"`
	UpdatedAt             int64   `json:"updatedAt"`
	UpdatedBy             ID      `json:"updatedBy"`
}

type AuditEntry struct {
	ID       ID      `json:"id"`
	ActorID  ID      `json:"actorId"`
	Action   string  `json:"action"`
	Target   string  `json:"target"`
	OldValue *string `json:"oldValue,omitempty"`
	NewValue *string `json:"newValue,omitempty"`
	At       int64   `json:"at"`
}

type WantedOffer struct {
	ID         ID     `json:"id"`
	ProviderID ID     `json:"providerId"`
	ListingID  *ID    `json:"listingId,omitempty"`
	Message    string `json:"message"`
	At         int64  `json:"at"`
}

type WantedPost struct {
	ID           ID            `json:"id"`
	RequesterID  ID            `json:"requesterId"`
	Title        string        `json:"title"`
	Description  string        `json:"description"`
	Category     *string       `json:"category,omitempty"`
	AreaID       string        `json:"areaId"`
	Location     GeoPoint      `json:"location"`
	RadiusKm     float64       `json:"radiusKm"`
	NeededFrom   *string       `json:"neededFrom,omitempty"`
	NeededUntil  *string       `json:"neededUntil,omitempty"`
	BudgetPerDay *int64        `json:"budgetPerDay,omitempty"`
	CreatedAt    int64         `json:"createdAt"`
	Status       string        `json:"status"` // terbuka | terpenuhi | ditutup
	Offers       []WantedOffer `json:"offers"`
}

type Notification struct {
	ID     ID      `json:"id"`
	UserID ID      `json:"userId"`
	Kind   string  `json:"kind"`
	Title  string  `json:"title"`
	Body   string  `json:"body"`
	Href   *string `json:"href,omitempty"`
	At     int64   `json:"at"`
	Read   bool    `json:"read"`
}

// Data is the full dataset in the exact shape of the web app's DataState.
type Data struct {
	Version         int64                  `json:"version"`
	Seeded          bool                   `json:"seeded"`
	SeedBase        int64                  `json:"seedBase"`
	ClockOffset     int64                  `json:"clockOffset"`
	Users           []*User                `json:"users"`
	Verifications   []*VerificationRequest `json:"verifications"`
	Listings        []*Listing             `json:"listings"`
	Bookings        []*Booking             `json:"bookings"`
	Photos          []*PhotoRecord         `json:"photos"`
	Messages        []*Message             `json:"messages"`
	Reviews         []*Review              `json:"reviews"`
	Reports         []*Report              `json:"reports"`
	Disputes        []*Dispute             `json:"disputes"`
	Strikes         []*Strike              `json:"strikes"`
	Commission      CommissionConfig       `json:"commission"`
	CollateralRules CollateralRules        `json:"collateralRules"`
	BannedKeywords  []string               `json:"bannedKeywords"`
	Audit           []*AuditEntry          `json:"audit"`
	Wanted          []*WantedPost          `json:"wanted"`
	Notifications   []*Notification        `json:"notifications"`
	Favorites       map[ID][]ID            `json:"favorites"`
	ChatReads       map[string]int64       `json:"chatReads"`
	BookingCounter  int                    `json:"bookingCounter"`
	// Scope is "full" (everything, demo fallback) or "scoped" (per viewer, ROADMAP §4); ViewerID is null for guests.
	Scope    string  `json:"scope"`
	ViewerID *string `json:"viewerId"`
	// ListingBusy: committed date ranges per listing in the snapshot (ACCEPTED/ACTIVE/DISPUTED, end ≥ today),
	// merged, with the number of units in use. No renter identity.
	ListingBusy map[ID][]BusyRange `json:"listingBusy"`
}

// BusyRange is a run of days on which Count units of a listing are booked.
type BusyRange struct {
	Start string `json:"start"` // YYYY-MM-DD (Asia/Jakarta)
	End   string `json:"end"`
	Count int    `json:"count"`
}
