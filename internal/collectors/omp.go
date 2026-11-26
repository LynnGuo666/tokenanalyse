package collectors

const (
	ompSourceID            = "omp"
	ompSourceApp           = "omp"
	ompProviderPlaceholder = "_omp_session"
)

var ompFlavor = piFamilyFlavor{
	sourceID:            ompSourceID,
	sourceApp:           ompSourceApp,
	name:                "Oh My Pi",
	providerPlaceholder: ompProviderPlaceholder,
	allowModelUsage:     true,
	titleSlot:           true,
}

// OMPCollector imports Oh My Pi sessions. The JSONL parser is shared with Pi,
// while source IDs, cursors, providers, and event identities remain distinct.
type OMPCollector struct{}

var _ Collector = OMPCollector{}

// OhMyPiCollector is a descriptive alias for OMPCollector.
type OhMyPiCollector = OMPCollector

func NewOMPCollector() *OMPCollector { return &OMPCollector{} }

func NewOhMyPiCollector() *OMPCollector { return &OMPCollector{} }

func (OMPCollector) Info() Info {
	return Info{
		ID:          ompSourceID,
		Name:        "Oh My Pi",
		AppTypes:    []string{ompSourceApp},
		Description: "Incremental Oh My Pi session JSONL collector",
		Incremental: true,
	}
}

func (OMPCollector) Collect(ctx Context, cursors []Cursor) (Result, error) {
	return collectPiFamily(ctx, cursors, ompFlavor)
}
