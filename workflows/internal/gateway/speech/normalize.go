package speech

import (
	"regexp"
	"strconv"
	"strings"
)

// docs/components/gateway/discord-voice.md's Notes Log — extending
// speech.SanitizeForSpeech (voice_text_sanitize.go) toward Vapi's "Voice Formatting
// Plan," a real production voice-AI platform's own answer to this exact
// problem class, found while researching whether a deterministic approach
// was actually the right one (it is — Vapi's own plan is "entirely
// rule-based, not ML-powered," 14 sequential deterministic steps). This
// file implements the subset of that reference actually built here:
// currency, percentages, clock times, email addresses, phone numbers, and
// newline/colon-to-period normalization.
//
// Deliberately NOT implemented, despite being part of Vapi's own plan —
// real gaps, not silently dropped:
//   - General arbitrary number-to-words (e.g. every standalone digit
//     sequence spoken as a number). Vapi does this; left out here because
//     the false-positive risk is much higher than everything below — a
//     version number, a turn ID, or any digit sequence that isn't actually
//     meant as a spoken quantity would get mangled, and unlike currency/
//     percent/time (unambiguous from their own punctuation), a bare number
//     has no local signal distinguishing "speak this as a quantity" from
//     "this happens to contain digits." Revisit if real usage shows plain
//     numbers are actually a live problem, not preemptively.
//   - Full date parsing (multiple calendar formats) — much larger scope
//     than the clock-time pattern below, no evidence yet it's a real
//     problem for this project's actual conversational usage.
//   - Acronym-casing normalization — Vapi's own described mechanism
//     ("lowercases known acronyms") depends on specifics of their TTS
//     engine's pronunciation behavior for all-caps text that aren't
//     verified against Kokoro specifically.
//   - Unit/measurement spelling beyond percent (kg, ft, mph, etc.) — a long
//     tail with no evidence yet of being a real problem here.
//
// Order matters and is enforced by speech.SanitizeForSpeech's own call sequence:
// clock times must be normalized before the generic colon-to-period pass
// below (a time's own colon would otherwise become a stray period first,
// and the time pattern would never match), and before markdown-symbol
// stripping strips anything overlapping.
func normalizeForSpeech(text string) string {
	text = replaceClockTimes(text)
	text = replacePhoneNumbers(text)
	text = replaceEmails(text)
	text = replaceCurrency(text)
	text = replacePercent(text)
	text = newlinePattern.ReplaceAllString(text, ". ")
	text = colonPattern.ReplaceAllString(text, ". ")
	return text
}

var (
	newlinePattern = regexp.MustCompile(`\n+`)
	// Only a colon followed by whitespace or end-of-string — a colon with a
	// digit on both sides (a time this function's own replaceClockTimes
	// didn't already consume, e.g. a genuinely unusual "3:2" ratio) is left
	// alone rather than guessed at.
	colonPattern = regexp.MustCompile(`:(\s|$)`)

	currencyPattern = regexp.MustCompile(`\$(\d+)(?:\.(\d{2}))?`)
	percentPattern  = regexp.MustCompile(`(\d+)%`)
	// H:MM, optionally with am/pm — deliberately not broader date parsing.
	// 24-hour hours (13-23) accepted too, spoken as their literal number
	// (matching how a person actually reads a 24-hour clock aloud) rather
	// than converted to 12-hour form.
	clockTimePattern = regexp.MustCompile(`\b([01]?\d|2[0-3]):([0-5]\d)(?:\s*([AaPp][Mm]))?\b`)
	emailPattern     = regexp.MustCompile(`\b([\w.+-]+)@([\w-]+\.[\w.-]+)\b`)
	// US-shaped phone numbers only: (555) 123-4567, 555-123-4567,
	// 555.123.4567 — real value, real scope limit, not an attempt at
	// international formats.
	phonePattern = regexp.MustCompile(`\(?\b(\d{3})\)?[-.\s](\d{3})[-.\s](\d{4})\b`)
)

func replaceClockTimes(text string) string {
	return clockTimePattern.ReplaceAllStringFunc(text, func(m string) string {
		sub := clockTimePattern.FindStringSubmatch(m)
		hour, _ := strconv.Atoi(sub[1])
		minute, _ := strconv.Atoi(sub[2])
		var minutePart string
		switch {
		case minute == 0:
			minutePart = "o'clock"
		case minute < 10:
			minutePart = "oh " + intToWords(minute)
		default:
			minutePart = intToWords(minute)
		}
		result := intToWords(hour) + " " + minutePart
		if sub[3] != "" {
			result += " " + strings.ToLower(sub[3])
		}
		return result
	})
}

func replaceEmails(text string) string {
	return emailPattern.ReplaceAllStringFunc(text, func(m string) string {
		sub := emailPattern.FindStringSubmatch(m)
		return sub[1] + " at " + strings.ReplaceAll(sub[2], ".", " dot ")
	})
}

// replacePhoneNumbers spaces out every digit individually — TTS engines
// (Kokoro included) read a bare, unspaced digit sequence as one large
// number ("5551234567" -> "five billion, five hundred fifty one million...")
// rather than a phone number; single space-separated digits read each one
// on its own instead.
func replacePhoneNumbers(text string) string {
	return phonePattern.ReplaceAllStringFunc(text, func(m string) string {
		var digits []string
		for _, r := range m {
			if r >= '0' && r <= '9' {
				digits = append(digits, string(r))
			}
		}
		return strings.Join(digits, " ")
	})
}

func replaceCurrency(text string) string {
	return currencyPattern.ReplaceAllStringFunc(text, func(m string) string {
		sub := currencyPattern.FindStringSubmatch(m)
		dollars, _ := strconv.Atoi(sub[1])
		result := intToWords(dollars) + " " + pluralizeWord(dollars, "dollar")
		if sub[2] != "" {
			if cents, _ := strconv.Atoi(sub[2]); cents > 0 {
				result += " and " + intToWords(cents) + " " + pluralizeWord(cents, "cent")
			}
		}
		return result
	})
}

func replacePercent(text string) string {
	return percentPattern.ReplaceAllStringFunc(text, func(m string) string {
		sub := percentPattern.FindStringSubmatch(m)
		n, _ := strconv.Atoi(sub[1])
		return intToWords(n) + " percent"
	})
}

func pluralizeWord(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

var (
	onesWords = [20]string{
		"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine",
		"ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen",
	}
	tensWords = [10]string{"", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"}
)

// intToWords converts a non-negative integer under one million into English
// words — enough range for any realistic currency amount, percentage, or
// clock hour/minute in casual conversation; this function's own callers
// never hand it anything larger. Not a general-purpose number-to-words
// library (no support for negatives, fractions, or values past 999,999) —
// this file's own doc comment has the reasoning for keeping general number
// verbalization out of scope entirely, this helper only serves the specific
// patterns above that already validated their input shape.
func intToWords(n int) string {
	switch {
	case n < 20:
		return onesWords[n]
	case n < 100:
		word := tensWords[n/10]
		if n%10 != 0 {
			word += "-" + onesWords[n%10]
		}
		return word
	case n < 1000:
		word := onesWords[n/100] + " hundred"
		if n%100 != 0 {
			word += " " + intToWords(n%100)
		}
		return word
	default:
		word := intToWords(n/1000) + " thousand"
		if n%1000 != 0 {
			word += " " + intToWords(n%1000)
		}
		return word
	}
}
