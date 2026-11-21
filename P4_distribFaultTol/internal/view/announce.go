package view

// Controllers for the announce node (registrar announcements). Like every
// other page, these reach the announce node only over gRPC; if it is down only
// these pages show an error.

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"p4/internal/authz"
	pb "p4/proto"
)

// dtLayout is what an <input type="datetime-local"> sends and expects.
const dtLayout = "2006-01-02T15:04"

var categories = []struct{ Value, Label string }{
	{"schedule", "Enrollment schedule"},
	{"confirmation", "Enrollment confirmation"},
	{"payment", "Payment deadline"},
	{"general", "General"},
}

func categoryLabel(v string) string {
	for _, c := range categories {
		if c.Value == v {
			return c.Label
		}
	}
	return v
}

func ordinal(y int32) string {
	switch y {
	case 1:
		return "1st"
	case 2:
		return "2nd"
	case 3:
		return "3rd"
	}
	return fmt.Sprintf("%dth", y)
}

// yearsLabel describes an audience: "Everyone" or e.g. "1st, 2nd year".
func yearsLabel(ys []int32) string {
	if len(ys) == 0 {
		return "Everyone"
	}
	parts := make([]string, len(ys))
	for i, y := range ys {
		parts[i] = ordinal(y)
	}
	return strings.Join(parts, ", ") + " year"
}

// when formats unix seconds for display in the configured timezone.
func (a *app) when(s int64) string {
	if s == 0 {
		return ""
	}
	return time.Unix(s, 0).In(a.cfg.Location).Format("Mon Jan 2, 2006 3:04 PM")
}

// inputDT formats unix seconds for a datetime-local input.
func (a *app) inputDT(s int64) string {
	if s == 0 {
		return ""
	}
	return time.Unix(s, 0).In(a.cfg.Location).Format(dtLayout)
}

// announcementForm is what the list and detail templates receive.
type announcementForm struct {
	List       []*pb.Announcement
	Item       *pb.Announcement
	Categories []struct{ Value, Label string }
	Years      []int
	CanEdit    bool
}

func (a *app) newForm(c *authz.Claims) announcementForm {
	return announcementForm{
		Categories: categories, Years: []int{1, 2, 3, 4},
		CanEdit: c.Role == authz.RoleRegistrar,
	}
}

// parseAnnouncement reads the create/edit form.
func (a *app) parseAnnouncement(r *http.Request) (*pb.Announcement, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	ann := &pb.Announcement{
		Category: r.FormValue("category"),
		Title:    r.FormValue("title"),
		Body:     r.FormValue("body"),
		Pinned:   r.FormValue("pinned") != "",
	}
	for _, v := range r.Form["year"] {
		y, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("bad year level %q", v)
		}
		ann.YearLevels = append(ann.YearLevels, int32(y))
	}
	for field, dst := range map[string]*int64{"starts_at": &ann.StartsAt, "ends_at": &ann.EndsAt} {
		v := strings.TrimSpace(r.FormValue(field))
		if v == "" {
			continue
		}
		t, err := time.ParseInLocation(dtLayout, v, a.cfg.Location)
		if err != nil {
			return nil, fmt.Errorf("bad date %q", v)
		}
		*dst = t.Unix()
	}
	return ann, nil
}

func (a *app) announcements(w http.ResponseWriter, r *http.Request) {
	s := sess(r)
	p := page{User: s.user, Msg: r.URL.Query().Get("msg"), Err: r.URL.Query().Get("err")}
	f := a.newForm(s.user)
	ctx, cancel := a.rpcCtx(r)
	defer cancel()
	res, err := a.announce.ListAnnouncements(ctx, &pb.Empty{})
	if err != nil {
		p.Err = errMsg("announce", err)
		f.CanEdit = false // no point offering a form the node can't take
	} else {
		f.List = res.Announcements
	}
	p.Data = f
	a.render(w, "announcements", p)
}

func (a *app) announcementPage(w http.ResponseWriter, r *http.Request) {
	s := sess(r)
	p := page{User: s.user, Msg: r.URL.Query().Get("msg"), Err: r.URL.Query().Get("err")}
	f := a.newForm(s.user)
	ctx, cancel := a.rpcCtx(r)
	defer cancel()
	item, err := a.announce.GetAnnouncement(ctx, &pb.AnnouncementId{Id: r.PathValue("id")})
	if err != nil {
		p.Err = errMsg("announce", err)
		f.CanEdit = false
	} else {
		f.Item = item
	}
	p.Data = f
	a.render(w, "announcement", p)
}

func (a *app) createAnnouncement(w http.ResponseWriter, r *http.Request) {
	ann, err := a.parseAnnouncement(r)
	if err != nil {
		http.Redirect(w, r, "/announcements?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	ctx, cancel := a.rpcCtx(r)
	defer cancel()
	if _, err := a.announce.CreateAnnouncement(ctx, ann); err != nil {
		http.Redirect(w, r, "/announcements?err="+url.QueryEscape(errMsg("announce", err)), http.StatusSeeOther)
		return
	}
	redirectMsg(w, r, "/announcements", "Announcement posted.")
}

func (a *app) updateAnnouncement(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	back := "/announcements/" + url.PathEscape(id)
	ann, err := a.parseAnnouncement(r)
	if err != nil {
		http.Redirect(w, r, back+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	ann.Id = id
	ctx, cancel := a.rpcCtx(r)
	defer cancel()
	if _, err := a.announce.UpdateAnnouncement(ctx, ann); err != nil {
		http.Redirect(w, r, back+"?err="+url.QueryEscape(errMsg("announce", err)), http.StatusSeeOther)
		return
	}
	redirectMsg(w, r, back, "Announcement updated.")
}

func (a *app) deleteAnnouncement(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, cancel := a.rpcCtx(r)
	defer cancel()
	if _, err := a.announce.DeleteAnnouncement(ctx, &pb.AnnouncementId{Id: id}); err != nil {
		http.Redirect(w, r, "/announcements/"+url.PathEscape(id)+"?err="+url.QueryEscape(errMsg("announce", err)), http.StatusSeeOther)
		return
	}
	redirectMsg(w, r, "/announcements", "Announcement deleted.")
}
