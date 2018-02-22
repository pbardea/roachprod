package cloud

import (
	"bytes"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"strings"
	"time"

	"github.com/cockroachdb/roachprod/config"
	"github.com/cockroachdb/roachprod/vm"
	"github.com/pkg/errors"
	"gopkg.in/gomail.v2"
)

// Tracks all the clusters to notify a user about.
type userNotification struct {
	Username string
	Good     []*CloudCluster
	Warning  []*CloudCluster
	Destroy  []*CloudCluster
	BadVMs   []string
}

// GCClusters checks all cluster to see if they should be deleted. It only
// fails on failure to perform cloud actions. All others actions (load/save
// file, email) do not abort.
func GCClusters(cloud *Cloud, filename string, destroyAfter time.Duration) error {
	trackedClusters := loadTrackingFile(filename)

	now := time.Now()
	destroyDeadline := now.Add(-destroyAfter)

	userActions := make(map[string]*userNotification)
	for _, c := range cloud.Clusters {
		if _, ok := userActions[c.User]; !ok {
			userActions[c.User] = &userNotification{Username: c.User}
		}

		actions := userActions[c.User]
		exp := c.ExpiresAt()

		if exp.After(now) {
			// Hasn't reached deadline yet.
			actions.Good = append(actions.Good, c)
		} else if exp.Before(destroyDeadline) {
			// Reached "destroy deadline".
			actions.Destroy = append(actions.Destroy, c)
		} else {
			// Expired, but not to be destroyed yet.
			actions.Warning = append(actions.Warning, c)
		}
	}

	// Compile list of "bad vms" and destroy them.
	badVMs := make(vm.List, 0)
	for _, vm := range cloud.BadInstances {
		// We only delete "bad vms" if they were created more than 1h ago.
		if now.Sub(vm.CreatedAt) >= time.Hour {
			badVMs = append(badVMs, vm)
		}
	}
	if len(badVMs) > 0 {
		err := vm.FanOut(badVMs, func(p vm.Provider, vms vm.List) error {
			return p.Delete(vms)
		})
		if err != nil {
			return errors.Wrapf(err, "failed to delete bad VMs")
		}
	}

	// Destroy expired clusters and build list of emails to send.
	warnedClusters := make([]string, 0)
	emails := make([]*gomail.Message, 0)
	for _, act := range userActions {
		needEmail := len(act.Destroy) > 0
		// Destroy marked clusters.
		for _, c := range act.Destroy {
			if err := DestroyCluster(c); err != nil {
				return errors.Wrapf(err, "failed to destroy cluster %s", c.Name)
			}
		}

		// Check if there are expired clusters we haven't warned about.
		for _, c := range act.Warning {
			if _, ok := trackedClusters[c.Name]; !ok {
				needEmail = true
			}
			warnedClusters = append(warnedClusters, c.Name)
		}

		if !needEmail {
			continue
		}

		act.BadVMs = badVMs.Names()
		e := buildEmail(act)
		if e != nil {
			emails = append(emails, e)
		}
	}

	writeTrackingFile(filename, warnedClusters)
	sendEmails(emails)
	return nil
}

func loadTrackingFile(filename string) map[string]interface{} {
	ret := make(map[string]interface{})

	content, err := ioutil.ReadFile(filename)
	// Don't fail on errors.
	if err != nil {
		log.Printf("Failed to read tracking file %s: %v", filename, err)
	}

	for _, cname := range strings.Split(string(content), "\n") {
		ret[cname] = nil
	}
	return ret
}

func writeTrackingFile(filename string, clusters []string) {
	err := ioutil.WriteFile(filename, []byte(strings.Join(clusters, "\n")), 0644)
	// Don't fail on errors.
	if err != nil {
		log.Printf("Failed to write tracking file %s: %v", filename, err)
	}
}

const (
	templateText = `
    <center>
    {{- $cloud := .}}

    {{if (len $cloud.Good) gt 0}}
      <h3>Good clusters</h3>
      <table border=1 style="border-collapse:collapse">
        <tr>
          <td>Name</td>
          <td>Nodes</td>
          <td>Expires</td>
        </tr> 

        {{- range $_, $c := $cloud.Good}}
          <tr>
            <td><b>{{$c.Name}}</b></td>
            <td>{{$c.VMs.Len}}</td>
            <td>{{$c.Expiration.Format "Mon, 02 Jan 2006 15:04:05 MST"}}</td>
          </tr> 
        {{- end}}
      </table>
      <br>
    {{end}}

    {{if (len $cloud.Warning) gt 0}}
      <h3>Expired clusters</h3>
      <table border=1 style="border-collapse:collapse">
        <tr>
          <td>Name</td>
          <td>Nodes</td>
          <td>Expires</td>
        </tr> 

        {{- range $_, $c := $cloud.Warning}}
          <tr>
            <td><b>{{$c.Name}}</b></td>
            <td>{{$c.VMs.Len}}</td>
            <td>{{$c.Expiration.Format "Mon, 02 Jan 2006 15:04:05 MST"}}</td>
          </tr> 
        {{- end}}
      </table>
      <br>
    {{end}}

    {{if (len $cloud.Destroy) gt 0}}
      <h3>Destroyed clusters</h3>
      <table border=1 style="border-collapse:collapse">
        <tr>
          <td>Name</td>
          <td>Nodes</td>
          <td>Expired</td>
        </tr> 

        {{- range $_, $c := $cloud.Destroy}}
          <tr>
            <td><b>{{$c.Name}}</b></td>
            <td>{{$c.VMs.Len}}</td>
            <td>{{$c.Expiration.Format "Mon, 02 Jan 2006 15:04:05 MST"}}</td>
          </tr> 
        {{- end}}
      </table>
      <br>
    {{end}}

    {{if (len $cloud.BadVMs) gt 0}}
      <h3>Destroyed bad VMs</h3>
      {{- range $_, $v := $cloud.BadVMs}}
        {{$v}}
        <br>
      {{- end}}
    {{end}}

    </center>
`
)

var emailTemplate = buildTemplate()

func buildTemplate() *template.Template {
	t, err := template.New("view").Parse(templateText)
	if err != nil {
		log.Fatalf("error parsing template: %v", err)
	}
	return t
}

func buildEmail(actions *userNotification) *gomail.Message {
	buf := new(bytes.Buffer)

	if err := emailTemplate.Execute(buf, actions); err != nil {
		log.Printf("could not execute template on %v: %v", actions, err)
		return nil
	}

	m := gomail.NewMessage()
	m.SetHeader("From", config.GCEmailOpts.From)
	m.SetHeader("To", fmt.Sprintf("%s%s", actions.Username, config.EmailDomain))
	m.SetHeader("Subject", time.Now().Format("Roachprod clusters 2006-01-02"))
	m.SetBody("text/html", buf.String())

	return m
}

func sendEmails(emails []*gomail.Message) {
	if len(config.GCEmailOpts.From) == 0 ||
		len(config.GCEmailOpts.Host) == 0 ||
		config.GCEmailOpts.Port == 0 ||
		len(config.GCEmailOpts.User) == 0 ||
		len(config.GCEmailOpts.Password) == 0 {
		log.Printf("you must specify all --email options to send email")
		return
	}

	dialer := gomail.NewDialer(
		config.GCEmailOpts.Host, config.GCEmailOpts.Port,
		config.GCEmailOpts.User, config.GCEmailOpts.Password)

	sender, err := dialer.Dial()
	if err != nil {
		log.Printf("could not dial SMTP server: %v", err)
		return
	}
	defer sender.Close()

	for _, e := range emails {
		if err := gomail.Send(sender, e); err != nil {
			log.Printf("failed to send email %+v: %v", err)
		} else {
			log.Printf("send email: %+v", e)
		}
	}
}
