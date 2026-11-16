package domain

// Action inputs: the exact JSON the web store passes as arguments.

type RegisterInput struct {
	Name        string     `json:"name"`
	Phone       string     `json:"phone"`
	Email       *string    `json:"email,omitempty"`
	AccountType string     `json:"accountType"`
	Store       *StoreInfo `json:"store,omitempty"`
	AreaID      string     `json:"areaId"`
}

type UserPatch struct {
	Name        *string    `json:"name,omitempty"`
	Nickname    *string    `json:"nickname,omitempty"`
	Email       *string    `json:"email,omitempty"`
	Bio         *string    `json:"bio,omitempty"`
	AreaID      *string    `json:"areaId,omitempty"`
	AccountType *string    `json:"accountType,omitempty"`
	Store       *StoreInfo `json:"store,omitempty"`
	Campus      *string    `json:"campus,omitempty"`
}

type VerificationInput struct {
	Method        string `json:"method"`
	NameOnKtp     string `json:"nameOnKtp"`
	Nik           string `json:"nik"`
	KtpPhotoID    *ID    `json:"ktpPhotoId,omitempty"`
	SelfiePhotoID *ID    `json:"selfiePhotoId,omitempty"`
}

type ListingInput struct {
	Title                *string            `json:"title,omitempty"`
	Category             *string            `json:"category,omitempty"`
	Description          *string            `json:"description,omitempty"`
	Photos               []string           `json:"photos,omitempty"`
	ConditionNotes       *string            `json:"conditionNotes,omitempty"`
	PricePerDay          *int64             `json:"pricePerDay,omitempty"`
	PricePerHour         *int64             `json:"pricePerHour,omitempty"`
	PricePerWeek         *int64             `json:"pricePerWeek,omitempty"`
	DurationDiscount     *DurationDiscount  `json:"durationDiscount,omitempty"`
	MinDays              *int               `json:"minDays,omitempty"`
	MaxDays              *int               `json:"maxDays,omitempty"`
	ItemValue            *int64             `json:"itemValue,omitempty"`
	CollateralOptions    []CollateralOption `json:"collateralOptions,omitempty"`
	Location             *ListingLocation   `json:"location,omitempty"`
	RadiusKm             *float64           `json:"radiusKm,omitempty"`
	Handover             []string           `json:"handover,omitempty"`
	MeetingPoint         *string            `json:"meetingPoint,omitempty"`
	HandoverHours        *string            `json:"handoverHours,omitempty"`
	Rules                *ListingRules      `json:"rules,omitempty"`
	Approval             *string            `json:"approval,omitempty"`
	AutoApproveMinRating *float64           `json:"autoApproveMinRating,omitempty"`
	Stock                *int               `json:"stock,omitempty"`
	Vehicle              *VehicleInfo       `json:"vehicle,omitempty"`
}

type BookingInput struct {
	ListingID          ID      `json:"listingId"`
	StartDate          string  `json:"startDate"`
	EndDate            string  `json:"endDate"`
	Handover           string  `json:"handover"`
	CollateralOptionID ID      `json:"collateralOptionId"`
	Note               *string `json:"note,omitempty"`
}

type CheckoutInput struct {
	PhotoIDs           []ID    `json:"photoIds"`
	CollateralPhotoIDs []ID    `json:"collateralPhotoIds"`
	Odometer           *string `json:"odometer,omitempty"`
	Fuel               *string `json:"fuel,omitempty"`
}

type CheckinInput struct {
	PhotoIDs        []ID    `json:"photoIds"`
	Result          string  `json:"result"`
	ProblemType     *string `json:"problemType,omitempty"`
	ProblemNote     *string `json:"problemNote,omitempty"`
	ProblemPhotoIDs []ID    `json:"problemPhotoIds,omitempty"`
	Odometer        *string `json:"odometer,omitempty"`
	Fuel            *string `json:"fuel,omitempty"`
}

type DisputeInput struct {
	CaseType    string `json:"caseType"`
	Description string `json:"description"`
	PhotoIDs    []ID   `json:"photoIds"`
}

type ProposalInput struct {
	Type   string  `json:"type"`
	Amount *int64  `json:"amount,omitempty"`
	Note   *string `json:"note,omitempty"`
}

type ReportInput struct {
	TargetType string  `json:"targetType"`
	TargetID   ID      `json:"targetId"`
	Reason     string  `json:"reason"`
	Detail     *string `json:"detail,omitempty"`
}

type StrikeInput struct {
	Reason    string `json:"reason"`
	Severity  string `json:"severity"`
	BookingID *ID    `json:"bookingId,omitempty"`
}

type CommissionPatch struct {
	Active             *bool    `json:"active,omitempty"`
	Mode               *string  `json:"mode,omitempty"`
	Percent            *float64 `json:"percent,omitempty"`
	Flat               *int64   `json:"flat,omitempty"`
	Min                *int64   `json:"min"`
	Max                *int64   `json:"max"`
	Bearer             *string  `json:"bearer,omitempty"`
	SplitRenterPercent *float64 `json:"splitRenterPercent,omitempty"`
	HasMin             bool     `json:"-"`
	HasMax             bool     `json:"-"`
}

type CollateralRulesPatch struct {
	LowMax                *int64   `json:"lowMax,omitempty"`
	MidMax                *int64   `json:"midMax,omitempty"`
	MidDepositPercent     *float64 `json:"midDepositPercent,omitempty"`
	HighDepositPercent    *float64 `json:"highDepositPercent,omitempty"`
	VehicleDepositPercent *float64 `json:"vehicleDepositPercent,omitempty"`
	HighValueBadge        *int64   `json:"highValueBadge,omitempty"`
}

type WantedInput struct {
	Title        string  `json:"title"`
	Description  string  `json:"description"`
	Category     *string `json:"category,omitempty"`
	AreaID       string  `json:"areaId"`
	RadiusKm     float64 `json:"radiusKm"`
	NeededFrom   *string `json:"neededFrom,omitempty"`
	NeededUntil  *string `json:"neededUntil,omitempty"`
	BudgetPerDay *int64  `json:"budgetPerDay,omitempty"`
}
