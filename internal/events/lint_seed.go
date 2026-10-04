package events

import "uuid"

// DefaultLintRules returns the starting KVKK lint rules: the rows migration
// 000060_seed_form_lint_rules writes (02 §11.5, R-003 §2.6, T-133). The
// in-memory store starts with them; TestStore_GormSeedMatchesDefaultLintRules
// keeps the two in step. Class choices follow 02 §11.4 (T-133): ID, card,
// password and account numbers and the special categories 02 does not list
// are blocked; health, disability, diet, blood group and religion are
// warn_special; passport, licence, tax and social security numbers are
// review_identifier.
func DefaultLintRules() []LintRule {
	return []LintRule{
		{ID: uuid.MustParse("01990000-0000-7000-8000-000000000001"), Class: LintBlocked, Active: true,
			Pattern:       `\btckn\b|\bt\.?\s?c\.?\s*(kimlik|vatandaslik|no\b|numara)|\bkimlik\s*(no\b|nu\b|numara)|\bvatandaslik\s*(no|numara)|\bnufus\s*cuzdan`,
			DescriptionTR: `T.C. kimlik, vatandaşlık veya nüfus cüzdanı numarası istenemez (veri minimizasyonu, KVKK md. 4).`},
		{ID: uuid.MustParse("01990000-0000-7000-8000-000000000002"), Class: LintBlocked, Active: true,
			Pattern:       `\b(kredi|banka|debit)\s*kart|\bkart\s*(no|numara|bilgi)|\bcvv\b|\bcvc\b|\bson\s*kullanma\s*tarih`,
			DescriptionTR: `Ödeme kartı bilgisi istenemez.`},
		{ID: uuid.MustParse("01990000-0000-7000-8000-000000000003"), Class: LintBlocked, Active: true,
			Pattern:       `\b(parola|sifre|password)|\bpin\s*kod`,
			DescriptionTR: `Parola, şifre veya PIN istenemez.`},
		{ID: uuid.MustParse("01990000-0000-7000-8000-000000000004"), Class: LintBlocked, Active: true,
			Pattern:       `\biban\b|\bhesap\s*(no|numara)|\bbanka\s*hesab`,
			DescriptionTR: `Banka hesabı veya IBAN istenemez.`},
		{ID: uuid.MustParse("01990000-0000-7000-8000-000000000005"), Class: LintBlocked, Active: true,
			Pattern:       `\bsabika|\bmahkumiyet|\bceza\s*(kayd|gecmis)|\badli\s*sicil`,
			DescriptionTR: `Sabıka veya mahkûmiyet bilgisi (özel nitelikli veri, KVKK md. 6).`},
		{ID: uuid.MustParse("01990000-0000-7000-8000-000000000006"), Class: LintBlocked, Active: true,
			Pattern:       `\bbiyometrik|\bparmak\s*izi|\bgenetik|\bdna\b|\bretina|\byuz\s*tanima`,
			DescriptionTR: `Biyometrik veya genetik veri (özel nitelikli veri, KVKK md. 6).`},
		{ID: uuid.MustParse("01990000-0000-7000-8000-000000000007"), Class: LintBlocked, Active: true,
			Pattern:       `\bsiyasi|\bsiyaset|\bparti\s*(uye|tercih)|\boy\s*(verdi|tercih)`,
			DescriptionTR: `Siyasi düşünce veya parti tercihi (özel nitelikli veri, KVKK md. 6).`},
		{ID: uuid.MustParse("01990000-0000-7000-8000-000000000008"), Class: LintBlocked, Active: true,
			Pattern:       `\betnik|\birk(i|iniz|im)?\b`,
			DescriptionTR: `Irk veya etnik köken (özel nitelikli veri, KVKK md. 6).`},
		{ID: uuid.MustParse("01990000-0000-7000-8000-000000000009"), Class: LintBlocked, Active: true,
			Pattern:       `\bcinsel\s*(yonelim|tercih|kimlik)|\blgbt`,
			DescriptionTR: `Cinsel yönelim veya cinsel hayat (özel nitelikli veri, KVKK md. 6).`},
		{ID: uuid.MustParse("01990000-0000-7000-8000-000000000010"), Class: LintBlocked, Active: true,
			Pattern:       `\bsendika|\b(dernek|vakif)\s*uyel`,
			DescriptionTR: `Sendika, dernek veya vakıf üyeliği (özel nitelikli veri, KVKK md. 6).`},
		{ID: uuid.MustParse("01990000-0000-7000-8000-000000000011"), Class: LintWarnSpecial, Active: true,
			Pattern:       `\bsaglik|\bhastalik|\bhasta\b|\balerji|\balerjen|\bilac|\bkronik|\brahatsizlik|\bameliyat|\btedavi|\bhamile|\bgebelik|\bpsikiyatr|\bdepresyon|\bpsikolojik\s*(destek|tedavi|sorun|durum|rahatsiz)`,
			DescriptionTR: `Sağlık bilgisi (özel nitelikli veri): gerekçeli istisna, ayrı açık rıza ve kısa saklama gerekir.`},
		{ID: uuid.MustParse("01990000-0000-7000-8000-000000000012"), Class: LintWarnSpecial, Active: true,
			Pattern:       `\bengelli|\bengel\s*durum|\bengeliniz\b|\bozel\s*ihtiyac|\berisilebilirlik|\btekerlekli\s*sandalye`,
			DescriptionTR: `Engellilik veya erişilebilirlik ihtiyacı (sağlık verisi): gerekçeli istisna gerekir.`},
		{ID: uuid.MustParse("01990000-0000-7000-8000-000000000013"), Class: LintWarnSpecial, Active: true,
			Pattern:       `\bdiyet|\bvegan|\bvejetaryen|\bhelal|\bglutensiz|\bgluten|\byemek\s*(tercih|ihtiyac|kisit)|\bozel\s*menu`,
			DescriptionTR: `Yemek tercihi veya diyet; dolaylı olarak din ya da sağlık bilgisi açığa çıkarabilir.`},
		{ID: uuid.MustParse("01990000-0000-7000-8000-000000000014"), Class: LintWarnSpecial, Active: true,
			Pattern:       `\bkan\s*grub`,
			DescriptionTR: `Kan grubu (sağlık verisi).`},
		{ID: uuid.MustParse("01990000-0000-7000-8000-000000000015"), Class: LintWarnSpecial, Active: true,
			Pattern:       `\bdin(i|sel|ini|iniz|im)?\b|\bmezhep|\binanc`,
			DescriptionTR: `Din, mezhep veya inanç (özel nitelikli veri, KVKK md. 6): gerekçeli istisna gerekir.`},
		{ID: uuid.MustParse("01990000-0000-7000-8000-000000000016"), Class: LintReviewIdentifier, Active: true,
			Pattern:       `\bpasaport|\bpassport`,
			DescriptionTR: `Pasaport numarası: yasal veya sigorta zorunluluğu yoksa istenmemeli.`},
		{ID: uuid.MustParse("01990000-0000-7000-8000-000000000017"), Class: LintReviewIdentifier, Active: true,
			Pattern:       `\behliyet\s*(no|numara)|\bsurucu\s*belge\s*(no|numara)`,
			DescriptionTR: `Ehliyet numarası: zorunlu değilse istenmemeli.`},
		{ID: uuid.MustParse("01990000-0000-7000-8000-000000000018"), Class: LintReviewIdentifier, Active: true,
			Pattern:       `\bvergi\s*(no|numara|kimlik)`,
			DescriptionTR: `Vergi numarası: zorunlu değilse istenmemeli.`},
		{ID: uuid.MustParse("01990000-0000-7000-8000-000000000019"), Class: LintReviewIdentifier, Active: true,
			Pattern:       `\bsgk\s*(no|sicil)|\bsosyal\s*guvenlik\s*(no|numara)`,
			DescriptionTR: `SGK veya sosyal güvenlik numarası: zorunlu değilse istenmemeli.`},
	}
}
