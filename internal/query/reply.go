package query

// Matching a reply to the question it answers, which is separate from recognizing either one.
//
// cm proxies only the queries its own model cannot answer, and it forwards the whole output stream to
// clients verbatim, including the queries it *does* answer. So a client's terminal sees questions cm never
// asked it and answers them, and those answers arrive on the same input path as the answer cm is waiting
// for. Deciding "is this the reply to what I asked" therefore cannot be "did it come from the client I
// asked": that was the reported `gh pr create --web` corruption, where a cursor report was accepted as a
// background colour.
//
// Kept in this package rather than in internal/input because it is the same knowledge as Classify, from the
// other direction: the shape of the reply a given query produces. Splitting the two invites them to
// disagree about a sequence, which is the failure that looks like ordinary output.

// AnswersQuery reports whether reply is an answer to query.
//
// Conservative by design, in the direction of *not* matching. An unrecognized reply is left for the request
// to expire, which costs the asking program one unanswered question, and that is exactly what happens today
// for every query cm cannot proxy. Matching wrongly is the expensive direction: it writes an answer to the
// wrong question, and a program reading positionally then takes it for the value it asked for.
func AnswersQuery(query, reply []byte) bool {
	switch {
	case isOSC(query):
		return oscAnswers(query, reply)
	case isDCS(query):
		// XTGETTCAP. The answer is a DCS too, either DCS 1 + r (found) or DCS 0 + r (not found), which is
		// the only DCS a client sends in reply to a proxied query, since DECRQSS is answered by cm's own
		// model and never proxied.
		return isDCS(reply)
	case isCSI(query):
		return csiAnswers(query, reply)
	case isAPC(query):
		return apcAnswers(query, reply)
	}
	return false
}

func isOSC(p []byte) bool { return len(p) >= 2 && p[0] == 0x1b && p[1] == ']' }
func isDCS(p []byte) bool { return len(p) >= 2 && p[0] == 0x1b && p[1] == 'P' }
func isCSI(p []byte) bool { return len(p) >= 2 && p[0] == 0x1b && p[1] == '[' }
func isAPC(p []byte) bool { return len(p) >= 3 && p[0] == 0x1b && p[1] == '_' && p[2] == 'G' }

// apcAnswers matches a kitty graphics response to the query it answers, by image id.
//
// The id carries the correspondence, and it has to be checked rather than accepting any graphics
// response: `kitten icat` asks three questions at once, one per transfer medium, and they are told apart
// only by i=. Accepting the first response for the first question would attribute an answer about shared
// memory to a question about a temp file, and icat would then use a medium the terminal rejected.
//
// A response carries no a= key, so there is nothing else to match on. One that names no id matches
// nothing, which leaves the request to expire rather than pairing it with a guess.
func apcAnswers(query, reply []byte) bool {
	if !isAPC(reply) {
		return false
	}
	qid, ok := graphicsID(query)
	if !ok {
		return false
	}
	rid, ok := graphicsID(reply)
	if !ok {
		return false
	}
	return string(qid) == string(rid)
}

// graphicsID reads the i= key from a graphics command or response.
func graphicsID(p []byte) (id []byte, ok bool) {
	end, _, found := apcEnd(p)
	if !found {
		return nil, false
	}
	body := p[3:end]
	if i := indexByte(body, ';'); i >= 0 {
		body = body[:i]
	}
	for _, kv := range splitByte(body, ',') {
		if len(kv) > 2 && kv[0] == 'i' && kv[1] == '=' {
			return kv[2:], true
		}
	}
	return nil, false
}

// oscAnswers matches an OSC reply to an OSC query by their numeric code.
//
// The code is what carries the correspondence: OSC 11 is answered by OSC 11, and a terminal that reports
// its background colour when asked for the foreground would be broken in a way cm cannot compensate for.
// Comparing codes rather than accepting any OSC is what keeps a clipboard read from being answered by a
// colour report, which matters because the two are asked together by some prompt hooks.
func oscAnswers(query, reply []byte) bool {
	if !isOSC(reply) {
		return false
	}
	_, qbody, ok := oscBody(query)
	if !ok {
		return false
	}
	_, rbody, ok := oscBody(reply)
	if !ok {
		return false
	}
	qn, ok := oscCode(qbody)
	if !ok {
		return false
	}
	rn, ok := oscCode(rbody)
	if !ok {
		return false
	}
	return qn == rn
}

// oscCode returns the numeric prefix of an OSC body, up to its first semicolon.
func oscCode(body []byte) (code string, ok bool) {
	i := 0
	for i < len(body) && isDigit(body[i]) {
		i++
	}
	if i == 0 {
		return "", false
	}
	return string(body[:i]), true
}

// csiAnswers matches a reply to the one CSI family cm proxies: the XTWINOPS size reports.
//
// CSI 14 t, 16 t, and 18 t ask for a size in pixels or cells, and each is answered by CSI with the same
// leading parameter: 4 for 14 t, 6 for 16 t, 8 for 18 t. The parameter is checked rather than accepting any
// CSI t, because a cursor position report also arrives unbidden on this path and CSI t is not the only thing
// a terminal volunteers.
func csiAnswers(query, reply []byte) bool {
	if !isCSI(reply) {
		return false
	}
	qparams, qinter, qfinal, _, ok := parseCSI(query)
	if !ok || qfinal != 't' || len(qinter) > 0 {
		return false
	}
	rparams, rinter, rfinal, _, ok := parseCSI(reply)
	if !ok || rfinal != 't' || len(rinter) > 0 {
		return false
	}
	want, ok := winopsReplyCode(leadingNumber(qparams))
	if !ok {
		return false
	}
	return leadingNumber(rparams) == want
}

// winopsReplyCode maps an XTWINOPS size request to the code its report carries.
func winopsReplyCode(req string) (code string, ok bool) {
	switch req {
	case "14":
		return "4", true
	case "16":
		return "6", true
	case "18":
		return "8", true
	}
	return "", false
}

// leadingNumber returns the digits at the start of a CSI parameter string.
func leadingNumber(params []byte) string {
	i := 0
	for i < len(params) && isDigit(params[i]) {
		i++
	}
	return string(params[:i])
}
