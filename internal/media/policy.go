package media

import "time"

// Photo stages. referensi = listing photo (public). The evidence stages belong to one booking.
const (
	StageReferensi = "referensi"
	StageCheckout  = "checkout"
	StageCheckin   = "checkin"
	StageJaminan   = "jaminan"
	StageMasalah   = "masalah"
	StageSengketa  = "sengketa"
	StageChat      = "chat"
)

var evidenceStages = map[string]bool{
	StageCheckout: true, StageCheckin: true, StageJaminan: true,
	StageMasalah: true, StageSengketa: true, StageChat: true,
}

// IsPublicStage reports whether photos of this stage are visible to everyone.
func IsPublicStage(stage string) bool { return stage == StageReferensi }

// IsEvidenceStage reports whether the stage is booking evidence (parties + admin only, FR-J5).
func IsEvidenceStage(stage string) bool { return evidenceStages[stage] }

// Viewer is who is asking. nil = guest.
type Viewer struct {
	ID    string
	Admin bool
}

// Photo is the metadata the media package needs about one photo row (core table `photos`).
type Photo struct {
	ID         string
	Stage      string
	BookingID  string // "" for listing photos
	ListingID  string
	UploaderID string
	ObjectKey  string // Storage key of the original, e.g. PhotoKey(id, ".jpg")
	SHA256     string // hex, computed at upload
	MIME       string
	Bytes      int64
	Width      int
	Height     int
	ServerTime time.Time // server timestamp at upload (FR-P2)
}

// Booking is the subset of a booking that access and retention decisions need.
type Booking struct {
	ID          string
	Code        string // human booking code shown in the watermark; falls back to ID
	RenterID    string
	ProviderID  string
	Status      string // REQUESTED … COMPLETED | RESOLVED | …
	CompletedAt *time.Time
	ResolvedAt  *time.Time
}

// CanView is the single access rule for photos served through /api/v1/photos/{id}:
//   - referensi (listing): everyone, guests included;
//   - checkout, checkin, jaminan, masalah, sengketa, chat: the booking's renter and provider, and admins;
//   - anything else (KYC "ktp"/"selfie", unknown stages): nobody. KYC files have their own audited admin
//     endpoint and never go through this path.
//
// booking must be the booking the photo belongs to; a mismatch or a missing booking denies non-admins.
func CanView(viewer *Viewer, photo Photo, booking *Booking) bool {
	if IsPublicStage(photo.Stage) {
		return true
	}
	if !IsEvidenceStage(photo.Stage) || viewer == nil || viewer.ID == "" {
		return false
	}
	if viewer.Admin {
		return true
	}
	if booking == nil || photo.BookingID == "" || booking.ID != photo.BookingID {
		return false
	}
	return viewer.ID == booking.RenterID || viewer.ID == booking.ProviderID
}
