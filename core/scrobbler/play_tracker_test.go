package scrobbler

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/navidrome/navidrome/consts"

	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/server/events"
	"github.com/navidrome/navidrome/tests"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("PlayTracker", func() {
	var ctx context.Context
	var ds model.DataStore
	var tracker PlayTracker
	var eventBroker *fakeEventBroker
	var track model.MediaFile
	var album model.Album
	var artist1 model.Artist
	var artist2 model.Artist
	var fake fakeScrobbler

	BeforeEach(func() {
		ctx = context.Background()
		ctx = request.WithUser(ctx, model.User{ID: "u-1"})
		ctx = request.WithPlayer(ctx, model.Player{ScrobbleEnabled: true})
		ds = &tests.MockDataStore{}
		fake = fakeScrobbler{Authorized: true}
		Register("fake", func(model.DataStore) Scrobbler {
			return &fake
		})
		Register("disabled", func(model.DataStore) Scrobbler {
			return nil
		})
		eventBroker = &fakeEventBroker{}
		tracker = newPlayTracker(ds, eventBroker)
		tracker.(*playTracker).scrobblers["fake"] = &fake // Bypass buffering for tests

		track = model.MediaFile{
			ID:             "123",
			Title:          "Track Title",
			Album:          "Track Album",
			AlbumID:        "al-1",
			TrackNumber:    1,
			Duration:       180,
			MbzRecordingID: "mbz-123",
			Participants: map[model.Role]model.ParticipantList{
				model.RoleArtist: []model.Participant{_p("ar-1", "Artist 1"), _p("ar-2", "Artist 2")},
			},
		}
		_ = ds.MediaFile(ctx).Put(&track)
		artist1 = model.Artist{ID: "ar-1"}
		_ = ds.Artist(ctx).Put(&artist1)
		artist2 = model.Artist{ID: "ar-2"}
		_ = ds.Artist(ctx).Put(&artist2)
		album = model.Album{ID: "al-1"}
		_ = ds.Album(ctx).(*tests.MockAlbumRepo).Put(&album)
	})

	It("does not register disabled scrobblers", func() {
		Expect(tracker.(*playTracker).scrobblers).To(HaveKey("fake"))
		Expect(tracker.(*playTracker).scrobblers).ToNot(HaveKey("disabled"))
	})

	Describe("NowPlaying", func() {
		It("sends track to agent", func() {
			err := tracker.NowPlaying(ctx, "player-1", "player-one", "123")
			Expect(err).ToNot(HaveOccurred())
			Expect(fake.NowPlayingCalled).To(BeTrue())
			Expect(fake.UserID).To(Equal("u-1"))
			Expect(fake.Track.ID).To(Equal("123"))
			Expect(fake.Track.Participants).To(Equal(track.Participants))
		})
		It("does not send track to agent if user has not authorized", func() {
			fake.Authorized = false

			err := tracker.NowPlaying(ctx, "player-1", "player-one", "123")

			Expect(err).ToNot(HaveOccurred())
			Expect(fake.NowPlayingCalled).To(BeFalse())
		})
		It("does not send track to agent if player is not enabled to send scrobbles", func() {
			ctx = request.WithPlayer(ctx, model.Player{ScrobbleEnabled: false})

			err := tracker.NowPlaying(ctx, "player-1", "player-one", "123")

			Expect(err).ToNot(HaveOccurred())
			Expect(fake.NowPlayingCalled).To(BeFalse())
		})
		It("does not send track to agent if artist is unknown", func() {
			track.Artist = consts.UnknownArtist

			err := tracker.NowPlaying(ctx, "player-1", "player-one", "123")

			Expect(err).ToNot(HaveOccurred())
			Expect(fake.NowPlayingCalled).To(BeFalse())
		})

		It("sends event with count", func() {
			err := tracker.NowPlaying(ctx, "player-1", "player-one", "123")
			Expect(err).ToNot(HaveOccurred())
			eventList := eventBroker.getEvents()
			Expect(eventList).ToNot(BeEmpty())
			evt, ok := eventList[0].(*events.NowPlayingCount)
			Expect(ok).To(BeTrue())
			Expect(evt.Count).To(Equal(1))
		})
	})

	Describe("GetNowPlaying", func() {
		It("returns current playing music", func() {
			track2 := track
			track2.ID = "456"
			_ = ds.MediaFile(ctx).Put(&track2)
			ctx = request.WithUser(context.Background(), model.User{UserName: "user-1"})
			_ = tracker.NowPlaying(ctx, "player-1", "player-one", "123")
			ctx = request.WithUser(context.Background(), model.User{UserName: "user-2"})
			_ = tracker.NowPlaying(ctx, "player-2", "player-two", "456")

			playing, err := tracker.GetNowPlaying(ctx)

			Expect(err).ToNot(HaveOccurred())
			Expect(playing).To(HaveLen(2))
			Expect(playing[0].PlayerId).To(Equal("player-2"))
			Expect(playing[0].PlayerName).To(Equal("player-two"))
			Expect(playing[0].Username).To(Equal("user-2"))
			Expect(playing[0].MediaFile.ID).To(Equal("456"))

			Expect(playing[1].PlayerId).To(Equal("player-1"))
			Expect(playing[1].PlayerName).To(Equal("player-one"))
			Expect(playing[1].Username).To(Equal("user-1"))
			Expect(playing[1].MediaFile.ID).To(Equal("123"))
		})
	})

	Describe("Expiration events", func() {
		It("sends event when entry expires", func() {
			info := NowPlayingInfo{MediaFile: track, Start: time.Now(), Username: "user"}
			_ = tracker.(*playTracker).playMap.AddWithTTL("player-1", info, 10*time.Millisecond)
			Eventually(func() int { return len(eventBroker.getEvents()) }).Should(BeNumerically(">", 0))
			eventList := eventBroker.getEvents()
			evt, ok := eventList[len(eventList)-1].(*events.NowPlayingCount)
			Expect(ok).To(BeTrue())
			Expect(evt.Count).To(Equal(0))
		})
	})

	Describe("Submit", func() {
		It("sends track to agent", func() {
			ctx = request.WithUser(ctx, model.User{ID: "u-1", UserName: "user-1"})
			ts := time.Now()

			err := tracker.Submit(ctx, []Submission{{TrackID: "123", Timestamp: ts}})

			Expect(err).ToNot(HaveOccurred())
			Expect(fake.ScrobbleCalled).To(BeTrue())
			Expect(fake.UserID).To(Equal("u-1"))
			Expect(fake.LastScrobble.ID).To(Equal("123"))
			Expect(fake.LastScrobble.Participants).To(Equal(track.Participants))
		})

		It("increments play counts in the DB", func() {
			ctx = request.WithUser(ctx, model.User{ID: "u-1", UserName: "user-1"})
			ts := time.Now()

			err := tracker.Submit(ctx, []Submission{{TrackID: "123", Timestamp: ts}})

			Expect(err).ToNot(HaveOccurred())
			Expect(track.PlayCount).To(Equal(int64(1)))
			Expect(album.PlayCount).To(Equal(int64(1)))

			// It should increment play counts for all artists
			Expect(artist1.PlayCount).To(Equal(int64(1)))
			Expect(artist2.PlayCount).To(Equal(int64(1)))
		})

		It("does not send track to agent if user has not authorized", func() {
			fake.Authorized = false

			err := tracker.Submit(ctx, []Submission{{TrackID: "123", Timestamp: time.Now()}})

			Expect(err).ToNot(HaveOccurred())
			Expect(fake.ScrobbleCalled).To(BeFalse())
		})

		It("does not send track to agent if player is not enabled to send scrobbles", func() {
			ctx = request.WithPlayer(ctx, model.Player{ScrobbleEnabled: false})

			err := tracker.Submit(ctx, []Submission{{TrackID: "123", Timestamp: time.Now()}})

			Expect(err).ToNot(HaveOccurred())
			Expect(fake.ScrobbleCalled).To(BeFalse())
		})

		It("does not send track to agent if artist is unknown", func() {
			track.Artist = consts.UnknownArtist

			err := tracker.Submit(ctx, []Submission{{TrackID: "123", Timestamp: time.Now()}})

			Expect(err).ToNot(HaveOccurred())
			Expect(fake.ScrobbleCalled).To(BeFalse())
		})

		It("increments play counts even if it cannot scrobble", func() {
			fake.Error = errors.New("error")

			err := tracker.Submit(ctx, []Submission{{TrackID: "123", Timestamp: time.Now()}})

			Expect(err).ToNot(HaveOccurred())
			Expect(fake.ScrobbleCalled).To(BeFalse())

			Expect(track.PlayCount).To(Equal(int64(1)))
			Expect(album.PlayCount).To(Equal(int64(1)))

			// It should increment play counts for all artists
			Expect(artist1.PlayCount).To(Equal(int64(1)))
			Expect(artist2.PlayCount).To(Equal(int64(1)))
		})
	})

})

type fakeScrobbler struct {
	Authorized       bool
	NowPlayingCalled bool
	ScrobbleCalled   bool
	UserID           string
	Track            *model.MediaFile
	LastScrobble     Scrobble
	Error            error
}

func (f *fakeScrobbler) IsAuthorized(ctx context.Context, userId string) bool {
	return f.Error == nil && f.Authorized
}

func (f *fakeScrobbler) NowPlaying(ctx context.Context, userId string, track *model.MediaFile) error {
	f.NowPlayingCalled = true
	if f.Error != nil {
		return f.Error
	}
	f.UserID = userId
	f.Track = track
	return nil
}

func (f *fakeScrobbler) Scrobble(ctx context.Context, userId string, s Scrobble) error {
	f.ScrobbleCalled = true
	if f.Error != nil {
		return f.Error
	}
	f.UserID = userId
	f.LastScrobble = s
	return nil
}

func _p(id, name string, sortName ...string) model.Participant {
	p := model.Participant{Artist: model.Artist{ID: id, Name: name}}
	if len(sortName) > 0 {
		p.Artist.SortArtistName = sortName[0]
	}
	return p
}

type fakeEventBroker struct {
	http.Handler
	events []events.Event
	mu     sync.Mutex
}

func (f *fakeEventBroker) SendMessage(_ context.Context, event events.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
}

func (f *fakeEventBroker) getEvents() []events.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.events
}

var _ events.Broker = (*fakeEventBroker)(nil)
