// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the parties module (03 §5.13), in table order.
const (
	PartiesLegalPartyRead     Action = "parties.legal_party.read"
	PartiesLegalPartyCreate   Action = "parties.legal_party.create"
	PartiesLegalPartyUpdate   Action = "parties.legal_party.update"
	PartiesLegalPartyVerify   Action = "parties.legal_party.verify"
	PartiesDocumentUpload     Action = "parties.document.upload"
	PartiesDocumentRead       Action = "parties.document.read"
	PartiesDocumentVerify     Action = "parties.document.verify"
	PartiesPayeeList          Action = "parties.payee.list"
	PartiesPayeeRead          Action = "parties.payee.read"
	PartiesPayeeManage        Action = "parties.payee.manage"
	PartiesPayeeVerify        Action = "parties.payee.verify"
	PartiesBankAccountList    Action = "parties.bank_account.list"
	PartiesBankAccountSubmit  Action = "parties.bank_account.submit"
	PartiesBankAccountVerify  Action = "parties.bank_account.verify"
	PartiesBankAccountApprove Action = "parties.bank_account.approve"
	PartiesBankAccountReject  Action = "parties.bank_account.reject"
	PartiesBankAccountRevoke  Action = "parties.bank_account.revoke"
	// PartiesIbanReveal is adminRevealBankAccountIban (BE-129): the audited
	// read of one full IBAN; every list and detail shows it masked.
	PartiesIbanReveal Action = "parties.iban.reveal"
)

// partiesActions lists the HTTP actions of parties in 03 §5.13 order.
var partiesActions = []Action{
	PartiesLegalPartyRead,
	PartiesLegalPartyCreate,
	PartiesLegalPartyUpdate,
	PartiesLegalPartyVerify,
	PartiesDocumentUpload,
	PartiesDocumentRead,
	PartiesDocumentVerify,
	PartiesPayeeList,
	PartiesPayeeRead,
	PartiesPayeeManage,
	PartiesPayeeVerify,
	PartiesBankAccountList,
	PartiesBankAccountSubmit,
	PartiesBankAccountVerify,
	PartiesBankAccountApprove,
	PartiesBankAccountReject,
	PartiesBankAccountRevoke,
	PartiesIbanReveal,
}
