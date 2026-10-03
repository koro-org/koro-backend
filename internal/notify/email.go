package notify

import (
	"strings"
	"time"
	"uuid"
)

// composeEmail assembles the provider-independent Email from a validated
// message and its rendered body. Inline images and attachments are copied, so
// a transport can never alias the caller's buffers; List-Unsubscribe is
// carried only for the commercial category.
func composeEmail(fromAddress, fromName string, id uuid.UUID, now time.Time, m Message, r Rendered) Email {
	e := Email{
		MessageID:   id.String() + "@" + domainOf(fromAddress),
		FromAddress: fromAddress,
		FromName:    fromName,
		To:          m.ToEmail,
		Date:        now,
		Subject:     r.Subject,
		Text:        r.Text,
		HTML:        r.HTML,
	}
	for _, in := range m.Inline {
		ct, _ := normalizeContentType(in.ContentType, inlineTypes) // validated
		e.Inline = append(e.Inline, InlineAttachment{CID: in.CID, ContentType: ct, Data: append([]byte(nil), in.Data...)})
	}
	for _, a := range m.Attachments {
		ct, _ := normalizeContentType(a.ContentType, attachmentTypes) // validated
		e.Attachments = append(e.Attachments, Attachment{Filename: a.Filename, ContentType: ct, Data: append([]byte(nil), a.Data...)})
	}
	if m.Category == CategoryCommercial {
		e.UnsubscribeURL = m.UnsubscribeURL
	}
	return e
}

func domainOf(addr string) string {
	return addr[strings.LastIndex(addr, "@")+1:]
}
