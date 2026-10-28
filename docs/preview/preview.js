// ReCON preview: a stand-in for ReCON's /api/* routes, so the pages in this
// folder run straight from GitHub Pages or a downloaded copy. Everything is
// sample data kept in memory; nothing is sent anywhere, and reloading a page
// starts it over. Rebuilt with: RECON_PREVIEW=1 go test -run TestPreview ./internal/web
(function () {
	'use strict';
	var GEN = window.RECON_PREVIEW_DATA; // data.js, written by TestPreview (internal/web)

	var NOW = Math.floor(Date.now() / 1000);
	var MIN = 60, HOUR = 3600, DAY = 86400;

	// ------------------------------------------------------------------
	// Sample players. GUIDs, IPs (documentation ranges) and SteamIDs are
	// invented; the SteamIDs are below Steam's first account number, so they
	// can't belong to anyone.
	// ------------------------------------------------------------------
	// guid makes a stable, made-up 32-character hex GUID from a name (FNV-1a
	// over the whole name, four times with different salts).
	function guid(seed) {
		var out = '';
		for (var block = 0; block < 4; block++) {
			var h = (2166136261 ^ Math.imul(block + 1, 0x9e3779b1)) >>> 0;
			for (var i = 0; i < seed.length; i++) {
				h ^= seed.charCodeAt(i);
				h = Math.imul(h, 16777619) >>> 0;
			}
			h ^= h >>> 13; h = Math.imul(h, 0x5bd1e995) >>> 0; h ^= h >>> 15;
			out += ('0000000' + h.toString(16)).slice(-8);
		}
		return out;
	}
	// Steam's first account is 76561197960265728; every sample SteamID is
	// below it, so none of them can be a real person's profile.
	function fakeSteamID(n) { return '765611979601' + ('0000' + (n * 4111 + 2087) % 100000).slice(-5); } // a string: too big for a JS number

	var players = [
		{ name: 'Brannigan', ip: '203.0.113.24', ping: 48, online: 0, role: 0, play: 41 * HOUR + 12 * MIN, first: 23, steam: 'found' },
		{ name: 'Kestrel', ip: '198.51.100.7', ping: 62, online: 1, role: 1, play: 96 * HOUR + 40 * MIN, first: 41, steam: 'found' },
		{ name: 'Oakshot', ip: '203.0.113.91', ping: 131, online: 2, role: 0, play: 7 * HOUR + 5 * MIN, first: 4, steam: 'found' },
		{ name: 'VeraLynn', ip: '192.0.2.15', ping: 35, online: 3, role: 2, play: 212 * HOUR + 3 * MIN, first: 58, steam: 'found' },
		{ name: 'Marrow', ip: '203.0.113.91', ping: 0, role: 0, play: 18 * HOUR + 30 * MIN, first: 9, last: 2 * HOUR + 14 * MIN, steam: 'found' },
		{ name: 'Dustcoat', ip: '198.51.100.212', ping: 0, role: 0, play: 3 * HOUR + 47 * MIN, first: 6, last: 5 * DAY + 3 * HOUR, steam: 'vac' },
		{ name: 'Fenwick', ip: '192.0.2.188', ping: 0, role: 0, play: 26 * HOUR + 9 * MIN, first: 30, last: 1 * DAY + 6 * HOUR, steam: 'found' },
		{ name: 'Survivor', ip: '198.51.100.66', ping: 0, role: 0, play: 3 * MIN, first: 0, last: 23 * MIN, steam: 'none' },
		{ name: 'Pike', ip: '192.0.2.40', ping: 0, role: 0, play: 64 * HOUR + 2 * MIN, first: 37, last: 3 * DAY + 11 * HOUR, steam: 'private' }
	];
	players.forEach(function (p, i) {
		p.guid = guid('recon-preview-' + p.name);
		p.id = p.online === undefined ? -1 : p.online;
		p.isOnline = p.online !== undefined;
		p.firstSeen = NOW - p.first * DAY - 5 * HOUR;
		p.lastSeen = p.isOnline ? NOW : NOW - p.last;
		p.port = String(2304 + i * 7);
		p.steamId = p.steam === 'none' ? '' : fakeSteamID(i);
		p.note = '';
	});
	function byName(n) { return players.filter(function (p) { return p.name === n; })[0]; }
	function byGuid(g) { g = String(g).toLowerCase(); return players.filter(function (p) { return p.guid === g; })[0]; }
	function online() { return players.filter(function (p) { return p.isOnline; }).sort(function (a, b) { return a.id - b.id; }); }

	byName('Marrow').note = 'Plays with Oakshot (same house IP). Warned for blocking the airfield gate on 10-12.';
	byName('Kestrel').note = 'Mod since the Namalsk wipe. Covers EU evenings.';
	byName('Fenwick').note = 'Combat logged three times in one evening; 3-day ban.';

	// The server's ban list. One entry is from an old bans.txt, for a GUID
	// ReCON has never seen, so it has no name.
	var oldGuid = guid('recon-preview-old-ban');
	var bans = [
		{ guid: byName('Dustcoat').guid, perm: true, minutes: 'perm', reason: 'Cheating (speed hack)' },
		{ guid: byName('Fenwick').guid, perm: false, minutes: String(2 * 1440 + 9 * 60 + 17), reason: 'Combat logging' },
		{ guid: oldGuid, perm: true, minutes: 'perm', reason: 'Imported from old bans.txt' }
	];
	function banOf(g) {
		var b = bans.filter(function (x) { return x.guid === g; })[0];
		if (!b) return { banned: false, perm: false, minutesLeft: 0, reason: '' };
		return { banned: true, perm: b.perm, minutesLeft: b.perm ? 0 : parseInt(b.minutes, 10), reason: b.reason };
	}

	// ------------------------------------------------------------------
	// Chat history and the console log
	// ------------------------------------------------------------------
	function iso(unix) { return new Date(unix * 1000).toISOString(); }
	var chats = {};
	function addChat(p, ago, dir, line) { (chats[p.guid] = chats[p.guid] || []).push({ time: iso(NOW - ago), direction: dir, line: line }); }
	addChat(byName('Brannigan'), 52 * MIN, 'in', '(Global) Brannigan: anyone selling a car battery?');
	addChat(byName('Brannigan'), 41 * MIN, 'in', '(Global) Brannigan: !players');
	addChat(byName('Brannigan'), 41 * MIN - 1, 'out', '> say 0 Online (4): #0 Brannigan, #1 Kestrel, #2 Oakshot, #3 VeraLynn');
	addChat(byName('Brannigan'), 9 * MIN, 'in', '(Global) Brannigan: !whisper 1 thanks for the help at the trader');
	addChat(byName('Oakshot'), 38 * MIN, 'in', '(Global) Oakshot: lag is rough tonight');
	addChat(byName('Oakshot'), 36 * MIN, 'out', '> say 2 Your ping is high, try the EU server if you are in Europe.');
	addChat(byName('Oakshot'), 3 * MIN, 'in', '(Direct) Oakshot: ok thanks');
	addChat(byName('Kestrel'), 30 * MIN, 'in', '(Global) Kestrel: !code');
	addChat(byName('VeraLynn'), 12 * MIN, 'in', '(Global) VeraLynn: restart at 18:00, wrap up your bases');
	addChat(byName('Survivor'), 25 * MIN, 'in', '(Global) Survivor: hello?');
	addChat(byName('Marrow'), 2 * HOUR + 30 * MIN, 'in', '(Global) Marrow: brb dinner');
	addChat(byName('Dustcoat'), 5 * DAY + 3 * HOUR + 4 * MIN, 'in', '(Global) Dustcoat: lol');
	addChat(byName('Fenwick'), 1 * DAY + 6 * HOUR + 20 * MIN, 'in', '(Side) Fenwick: gg');

	var events = [], seq = 0;
	function ev(ago, kind, text) { events.push({ seq: ++seq, time: iso(NOW - ago), kind: kind, text: text, playerId: -1 }); }
	ev(58 * MIN, 'admin', 'RCon admin #0 (10.0.0.5:51022) logged in');
	ev(55 * MIN, 'connect', 'Player #0 Brannigan (203.0.113.24:2304) connected');
	ev(55 * MIN - 2, 'guid', 'Player #0 Brannigan - BE GUID: ' + byName('Brannigan').guid);
	ev(52 * MIN, 'chat', '(Global) Brannigan: anyone selling a car battery?');
	ev(47 * MIN, 'connect', 'Player #1 Kestrel (198.51.100.7:2311) connected');
	ev(47 * MIN - 2, 'guid', 'Player #1 Kestrel - BE GUID: ' + byName('Kestrel').guid);
	ev(44 * MIN, 'connect', 'Player #2 Oakshot (203.0.113.91:2318) connected');
	ev(44 * MIN - 2, 'guid', 'Player #2 Oakshot - BE GUID: ' + byName('Oakshot').guid);
	ev(38 * MIN, 'chat', '(Global) Oakshot: lag is rough tonight');
	ev(31 * MIN, 'connect', 'Player #3 VeraLynn (192.0.2.15:2325) connected');
	ev(31 * MIN - 2, 'guid', 'Player #3 VeraLynn - BE GUID: ' + byName('VeraLynn').guid);
	ev(26 * MIN, 'connect', 'Player #4 Survivor (198.51.100.66:2353) connected');
	ev(23 * MIN, 'kick', 'Player #4 Survivor (' + byName('Survivor').guid + ') has been kicked by BattlEye: Admin Kick (Name must not be Survivor)');
	ev(12 * MIN, 'chat', '(Global) VeraLynn: restart at 18:00, wrap up your bases');
	ev(9 * MIN, 'chat', '(Global) Brannigan: !whisper 1 thanks for the help at the trader');
	ev(3 * MIN, 'chat', '(Direct) Oakshot: ok thanks');

	// ------------------------------------------------------------------
	// Steam profiles
	// ------------------------------------------------------------------
	function avatar(name) {
		var hues = [210, 140, 30, 280, 0, 180, 50, 320, 100];
		var h = hues[name.length % hues.length];
		var svg = '<svg xmlns="http://www.w3.org/2000/svg" width="64" height="64"><rect width="64" height="64" fill="hsl(' + h + ',35%,28%)"/>' +
			'<text x="32" y="42" font-family="sans-serif" font-size="28" fill="hsl(' + h + ',60%,80%)" text-anchor="middle">' + name[0] + '</text></svg>';
		return 'data:image/svg+xml;charset=utf-8,' + encodeURIComponent(svg);
	}
	function steamProfile(p) {
		var age = { Brannigan: 6.2, Kestrel: 9.5, Oakshot: 0.4, VeraLynn: 11.1, Marrow: 3.8, Dustcoat: 0.1, Fenwick: 5.0, Pike: 7.7 }[p.name] || 4;
		var pr = {
			steamId: p.steamId, profileUrl: 'https://steamcommunity.com/profiles/' + p.steamId, found: true,
			personaName: p.name === 'VeraLynn' ? 'Vera' : p.name.toLowerCase(), avatarUrl: avatar(p.name),
			public: p.steam !== 'private', status: p.isOnline ? 'Online' : 'Offline', inGame: p.isOnline ? 'DayZ' : undefined,
			countryCode: p.steam === 'private' ? '' : ({ Brannigan: 'US', Kestrel: 'DE', Oakshot: 'GB', VeraLynn: 'CA', Marrow: 'GB', Dustcoat: 'RU', Fenwick: 'US' }[p.name] || ''),
			createdUnix: p.steam === 'private' ? 0 : NOW - Math.round(age * 365 * DAY),
			lastOnlineUnix: p.steam === 'private' ? 0 : p.lastSeen + 20 * MIN,
			vacBanned: p.steam === 'vac', vacBans: p.steam === 'vac' ? 1 : 0, gameBans: p.steam === 'vac' ? 2 : 0,
			daysSinceLastBan: p.steam === 'vac' ? 41 : 0, communityBanned: false, economyBan: 'none',
			level: p.steam === 'private' ? null : ({ Brannigan: 23, Kestrel: 41, Oakshot: 3, VeraLynn: 67, Marrow: 12, Dustcoat: 0, Fenwick: 18 }[p.name] || 5),
			gamesOwned: p.steam === 'private' ? null : ({ Oakshot: 4, Dustcoat: 2 }[p.name] || 40 + p.name.length * 13),
			dayzOwned: p.steam === 'private' ? null : p.name !== 'Dustcoat',
			dayzMinutes: Math.round(p.play / 60) + ({ VeraLynn: 140000, Kestrel: 61000, Brannigan: 18500 }[p.name] || 2400),
			dayz2WeeksMinutes: p.isOnline ? 900 + p.name.length * 60 : (p.name === 'Marrow' ? 600 : 0),
			dayzLastPlayedUnix: p.lastSeen,
			friends: p.steam === 'private' ? null : 12 + p.name.length * 4,
			fetchedUnix: NOW - 4 * MIN
		};
		if (pr.dayzOwned === false) pr.dayzMinutes = 0;
		return pr;
	}
	var steamFriends = { Brannigan: ['Kestrel'], Kestrel: ['Brannigan', 'VeraLynn'], VeraLynn: ['Kestrel'], Oakshot: ['Marrow'], Marrow: ['Oakshot'] };
	var searches = {}; // guid -> {startedAt}
	function steamView(p) {
		if (!p.steamId) {
			var s = searches[p.guid];
			if (s) {
				var pct = Math.min(100, (Date.now() - s.startedAt) / 120);
				if (pct >= 100) {
					p.steamId = fakeSteamID(20);
					p.steam = 'found';
					delete searches[p.guid];
					return steamView(p);
				}
				return { ok: true, enabled: true, steamId: '', pending: true,
					search: { state: 'searching', percent: pct, cores: 4, priority: 'high', startedUnix: Math.floor(s.startedAt / 1000), searchingFor: 1 } };
			}
			return { ok: true, enabled: true, steamId: '', pending: true, search: { state: 'idle', auto: true } };
		}
		return { ok: true, enabled: true, steamId: p.steamId, pending: false, profileUrl: 'https://steamcommunity.com/profiles/' + p.steamId,
			profile: steamProfile(p),
			friendsHere: (steamFriends[p.name] || []).map(function (n) { var o = byName(n); return { guid: o.guid, name: o.name }; }) };
	}

	// ------------------------------------------------------------------
	// Flows: the three in examples/flows.json, plus a Player message flow
	// and a Manual one, so every trigger shows.
	// ------------------------------------------------------------------
	var flows = JSON.parse(JSON.stringify(GEN.examples.flows));
	flows.push({
		name: 'Discord link', trigger: 'chat', steps: [{ type: 'if', branches: [
			{ subject: 'message', op: 'starts', value: '!discord', steps: [{ type: 'message', to: 'player', text: 'Join us on Discord: discord.gg/example' }] }
		] }]
	});
	flows.push({
		name: 'Restart in 10 minutes', trigger: 'manual', steps: [
			{ type: 'message', to: 'all', text: 'Unscheduled restart in 10 minutes.' },
			{ type: 'wait', time: '5m' },
			{ type: 'message', to: 'all', text: 'Unscheduled restart in 5 minutes.' },
			{ type: 'wait', time: '4m' },
			{ type: 'lock' },
			{ type: 'kickAll' },
			{ type: 'wait', time: '30s' },
			{ type: 'shutdown' }
		], onStop: [{ type: 'message', to: 'all', text: 'Restart cancelled.' }, { type: 'unlock' }]
	});
	var nextFlowId = 1;
	flows.forEach(function (f) {
		f.id = nextFlowId++;
		f.schedule = f.schedule || '';
		f.delay = f.delay || '';
		f.onStop = f.onStop || [];
		f.scheduleEnabled = f.trigger !== 'manual';
	});
	function flowNamed(n) { return flows.filter(function (f) { return f.name === n; })[0]; }

	function clock(unix) { var d = new Date(unix * 1000); return pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds()); }
	function pad(n) { return String(n).padStart(2, '0'); }
	var runs = {};
	function run(f, status, active, started, anchor, lines) {
		runs[f.id] = { flowId: f.id, name: f.name, status: status, active: active, anchorUnix: anchor, lastRun: started,
			log: lines.map(function (l) { return clock(l[0]) + '  ' + l[1]; }) };
	}
	(function () {
		var w = flowNamed('Welcome Player'), m = flowNamed('MOTD'), d = flowNamed('Discord link');
		var t = NOW - 26 * MIN;
		run(w, 'done', false, t, t + 3, [
			[NOW - 55 * MIN, 'Started by player join trigger'],
			[NOW - 55 * MIN, 'player 0 (Brannigan) joined'], [NOW - 55 * MIN, 'waiting 3s before step 1'],
			[NOW - 55 * MIN + 3, 'If {player_name} == Survivor: no'], [NOW - 55 * MIN + 3, 'Else'],
			[NOW - 55 * MIN + 3, 'sent "say 0 Welcome Brannigan to our DayZ server!" (acked=true err=<nil>)'],
			[NOW - 55 * MIN + 3, 'done for Brannigan (#0)'],
			[t, 'Started by player join trigger'],
			[t, 'player 4 (Survivor) joined'], [t, 'waiting 3s before step 1'], [t + 3, 'If {player_name} == Survivor: yes'],
			[t + 3, 'sent "kick 4 Name must not be Survivor" (acked=true err=<nil>)'], [t + 3, 'done for Survivor (#4)']
		]);
		var last = Math.floor(NOW / 1200) * 1200;
		run(m, 'done', false, last, last + 3, [
			[last, 'Started by schedule'], [last, 'waiting 3s before step 1'],
			[last + 3, 'sent "say -1 Admins are active, please play respectfully." (acked=true err=<nil>)'],
			[last + 5, 'sent "say -1 If you need admin assistance, send a message in game or type !help." (acked=true err=<nil>)'],
			[last + 5, 'done']
		]);
		run(d, 'done', false, NOW - 6 * MIN, NOW - 6 * MIN, [
			[NOW - 9 * MIN, 'Started by player message trigger'],
			[NOW - 9 * MIN, 'Brannigan: !whisper 1 thanks for the help at the trader'], [NOW - 9 * MIN, 'If {message} starts with !discord: no'],
			[NOW - 9 * MIN, 'done for Brannigan (#0)'],
			[NOW - 6 * MIN, 'Started by player message trigger'],
			[NOW - 6 * MIN, 'Oakshot: !discord'], [NOW - 6 * MIN, 'If {message} starts with !discord: yes'],
			[NOW - 6 * MIN, 'sent "say 2 Join us on Discord: discord.gg/example" (acked=true err=<nil>)'], [NOW - 6 * MIN, 'done for Oakshot (#2)']
		]);
	})();

	// Cron: enough to list the next runs in the browser. Descriptions come
	// from ReCON's own describe() for the schedules used here.
	function cronField(s, lo, hi, names) {
		var set = {};
		var ok = s.split(',').every(function (part) {
			var m = /^(\*|[a-z0-9]+(?:-[a-z0-9]+)?)(?:\/(\d+))?$/i.exec(part);
			if (!m) return false;
			var step = m[2] ? parseInt(m[2], 10) : 1, a = lo, b = hi;
			function num(x) {
				var i = names ? names.indexOf(x.toLowerCase()) : -1;
				return i >= 0 ? i + lo : (/^\d+$/.test(x) ? parseInt(x, 10) : NaN);
			}
			if (m[1] !== '*') {
				var r = m[1].split('-');
				a = num(r[0]); b = r.length > 1 ? num(r[1]) : (m[2] ? hi : a);
			}
			if (isNaN(a) || isNaN(b) || a < lo || b > hi + (hi === 6 ? 1 : 0) || a > b || step < 1) return false;
			for (var v = a; v <= b; v += step) set[hi === 6 && v === 7 ? 0 : v] = true;
			return true;
		});
		return ok ? set : null;
	}
	function parseCron(expr) {
		var f = String(expr || '').trim().split(/\s+/);
		if (f.length !== 5) return null;
		var c = [cronField(f[0], 0, 59), cronField(f[1], 0, 23), cronField(f[2], 1, 31),
			cronField(f[3], 1, 12, ['jan', 'feb', 'mar', 'apr', 'may', 'jun', 'jul', 'aug', 'sep', 'oct', 'nov', 'dec']),
			cronField(f[4], 0, 6, ['sun', 'mon', 'tue', 'wed', 'thu', 'fri', 'sat'])];
		if (c.some(function (x) { return !x; })) return null;
		c.domStar = f[2] === '*'; c.dowStar = f[4] === '*';
		return c;
	}
	function cronNext(c, after) {
		var d = new Date((Math.floor(after / 60) + 1) * 60000);
		for (var i = 0; i < 60 * 24 * 400; i++) {
			var dom = c[2][d.getDate()], dow = c[4][d.getDay()];
			var day = c.domStar || c.dowStar ? (dom && dow) : (dom || dow);
			if (c[0][d.getMinutes()] && c[1][d.getHours()] && c[3][d.getMonth() + 1] && day) return Math.floor(d / 1000);
			d = new Date(d.getTime() + 60000);
		}
		return 0;
	}
	function secondsOf(t) {
		t = String(t || '').trim();
		if (!t) return 0;
		if (/^\d+$/.test(t)) return parseInt(t, 10);
		var m = /^(?:(\d+)h)?\s*(?:(\d+)m)?\s*(?:(\d+)s)?$/.exec(t.replace(/\s+/g, ''));
		if (!m || !(m[1] || m[2] || m[3])) return NaN;
		return (+m[1] || 0) * 3600 + (+m[2] || 0) * 60 + (+m[3] || 0);
	}
	var tzName = (function () {
		try { return new Intl.DateTimeFormat('en-US', { timeZoneName: 'short' }).formatToParts(new Date()).filter(function (x) { return x.type === 'timeZoneName'; })[0].value; } catch (e) { return ''; }
	})();
	function fmtWhen(unix) { // Go: "Mon 02 Jan 15:04 MST"
		var d = new Date(unix * 1000);
		var days = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'], mons = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
		return days[d.getDay()] + ' ' + pad(d.getDate()) + ' ' + mons[d.getMonth()] + ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes()) + (tzName ? ' ' + tzName : '');
	}
	function flowList() {
		var now = Date.now() / 1000;
		return flows.map(function (f) {
			var o = JSON.parse(JSON.stringify(f));
			if (runs[f.id]) o.lastRun = fmtWhen(runs[f.id].lastRun);
			if (f.trigger === 'manual') o.nextRun = 'Manual only';
			else if (f.scheduleEnabled) {
				if (f.trigger === 'join') o.nextRun = 'On player join';
				else if (f.trigger === 'chat') o.nextRun = 'On player message';
				else { var c = parseCron(f.schedule); if (c) o.nextRun = fmtWhen(cronNext(c, now) + (secondsOf(f.delay) || 0)); }
			}
			return o;
		});
	}
	function startRun(f, by) {
		var now = Math.floor(Date.now() / 1000), delay = secondsOf(f.delay) || 0;
		var lines = [[now, 'Started by ' + by]];
		if (delay) lines.push([now, 'waiting ' + f.delay + ' before step 1']);
		var first = f.steps[0];
		if (!delay && first && first.type === 'message') lines.push([now, 'sent "say -1 ' + first.text + '" (acked=true err=<nil>)']);
		run(f, 'running', true, now, now + delay, lines);
		runs[f.id].log.push(clock(now) + '  (preview: nothing is sent; this run only counts down)');
	}

	// ------------------------------------------------------------------
	// Chat commands settings
	// ------------------------------------------------------------------
	var chatSaved = JSON.parse(JSON.stringify(GEN.defaults));

	// ------------------------------------------------------------------
	// Routes
	// ------------------------------------------------------------------
	function json(body, status) {
		return Promise.resolve(new Response(JSON.stringify(body), { status: status || 200, headers: { 'Content-Type': 'application/json' } }));
	}
	function fail(status, msg) { return json({ ok: false, error: msg }, status); }
	function body(opt) { try { return opt && opt.body ? JSON.parse(opt.body) : {}; } catch (e) { return {}; } }
	function playerJSON(p) {
		return { id: p.id, ip: p.ip, port: p.port, ping: p.ping, guid: p.guid, name: p.name, offline: !p.isOnline,
			totalPlaytimeSeconds: p.play, lastSeenUnix: p.isOnline ? 0 : p.lastSeen, role: p.role };
	}
	function banMap() { var m = {}; bans.forEach(function (b) { m[b.guid] = banOf(b.guid); }); return m; }
	function sharedIP(p) {
		return players.filter(function (o) { return o !== p && o.ip === p.ip; }).map(function (o) { return { guid: o.guid, name: o.name }; });
	}
	function details(p) {
		var sessions = [], n = Math.max(1, Math.min(12, Math.round(p.play / (2.4 * HOUR))));
		var t = p.isOnline ? NOW - (20 + p.name.length * 3) * MIN : p.lastSeen;
		if (p.isOnline) sessions.push({ connected: t, disconnected: 0, seconds: NOW - t, ip: p.ip, current: true });
		var each = Math.floor(p.play / n);
		for (var i = 0; i < n; i++) {
			var end = p.isOnline ? t - (i + 1) * 20 * HOUR : t - i * 20 * HOUR;
			sessions.push({ connected: end - each, disconnected: end, seconds: each, ip: i === 3 ? '192.0.2.' + (40 + p.name.length) : p.ip, current: false });
		}
		var ips = [{ ip: p.ip, firstUsed: p.firstSeen, lastUsed: p.lastSeen, others: sharedIP(p) }];
		if (n > 3) ips.push({ ip: '192.0.2.' + (40 + p.name.length), firstUsed: p.firstSeen + 2 * DAY, lastUsed: p.firstSeen + 2 * DAY, others: [] });
		return {
			player: { guid: p.guid, name: p.name, lastIp: p.ip, firstSeen: p.firstSeen, lastSeen: p.lastSeen, playtimeSeconds: p.play,
				online: p.isOnline, id: p.id, role: p.role },
			sessions: { count: n, avgSeconds: each, recent: sessions },
			ips: ips, ban: banOf(p.guid), note: { text: p.note, updated: p.note ? NOW - 3 * DAY : 0 }
		};
	}
	function kick(p, reason) {
		var now = Math.floor(Date.now() / 1000);
		events.push({ seq: ++seq, time: iso(now), kind: 'kick', text: 'Player #' + p.id + ' ' + p.name + ' (' + p.guid + ') has been kicked by BattlEye: Admin Kick (' + reason + ')', playerId: p.id });
		p.isOnline = false; p.lastSeen = now; p.id = -1;
	}
	function ban(p, reason, minutes) {
		bans = bans.filter(function (b) { return b.guid !== p.guid; });
		bans.push({ guid: p.guid, perm: !minutes, minutes: minutes ? String(minutes) : 'perm', reason: reason || 'No reason specified' });
		var wasOnline = p.isOnline;
		if (wasOnline) kick(p, reason || 'No reason specified');
		return json({ ok: true, guid_ban: true, db_updated: true, online: wasOnline, kicked: wasOnline, perm: !minutes });
	}
	function consoleReply(cmd) {
		var low = cmd.toLowerCase();
		if (low === 'players') {
			var on = online();
			return 'Players on server:\n[#] [IP Address]:[Port] [Ping] [GUID] [Name]\n--------------------------------------------------\n' +
				on.map(function (p) { return p.id + '   ' + p.ip + ':' + p.port + '  ' + p.ping + '   ' + p.guid + '(OK) ' + p.name; }).join('\n') +
				'\n(' + on.length + ' players in total)';
		}
		if (low === 'bans') {
			return 'GUID Bans:\n[#] [GUID] [Minutes left] [Reason]\n----------------------------------------\n' +
				bans.map(function (b, i) { return i + '  ' + b.guid + ' ' + b.minutes + ' ' + b.reason; }).join('\n');
		}
		if (low === 'admins') return 'Connected RCon admins:\n[#] [IP Address]:[Port]\n-----------------------------\n0 10.0.0.5:51022';
		return '';
	}

	var routes = [
		['GET', /^\/health$/, function () { return json({ ok: true }); }],
		['GET', /^\/api\/status$/, function () { return json({ ok: true, rcon: 'connected' }); }],

		['GET', /^\/api\/players$/, function () {
			var on = online();
			return json({ ok: true, connected: true, players: on.map(playerJSON), count: on.length,
				offlinePlayers: players.filter(function (p) { return !p.isOnline; }).sort(function (a, b) { return b.lastSeen - a.lastSeen; }).map(playerJSON),
				bans: banMap() });
		}],
		['GET', /^\/api\/players\/all$/, function () {
			return json({ ok: true, players: players.map(playerJSON), count: players.length });
		}],
		['GET', /^\/api\/players\/([0-9a-f]{32})\/details$/, function (m) {
			var p = byGuid(m[1]);
			return p ? json({ ok: true, details: details(p) }) : fail(404, 'unknown player (this GUID has never been seen on the server)');
		}],
		['GET', /^\/api\/players\/([0-9a-f]{32})\/chat$/, function (m) { return json({ ok: true, messages: chats[m[1]] || [] }); }],
		['POST', /^\/api\/players\/([0-9a-f]{32})\/chat$/, function (m, o) {
			var p = byGuid(m[1]), msg = String(body(o).message || '').trim();
			if (!msg) return fail(400, 'message is required');
			if (!p || !p.isOnline) return fail(409, 'player is no longer connected');
			(chats[p.guid] = chats[p.guid] || []).push({ time: new Date().toISOString(), direction: 'out', line: '> say ' + p.id + ' ' + msg });
			return json({ ok: true });
		}],
		['POST', /^\/api\/players\/([0-9a-f]{32})\/note$/, function (m, o) {
			var p = byGuid(m[1]), note = String(body(o).note || '').replace(/\s+$/, '');
			if (note.length > 500) return fail(400, 'note is ' + note.length + ' characters; the limit is 500');
			p.note = note;
			return json({ ok: true, note: note });
		}],
		['POST', /^\/api\/players\/([0-9a-f]{32})\/role$/, function (m, o) {
			var p = byGuid(m[1]), r = body(o).role;
			if ([0, 1, 2].indexOf(r) < 0) return fail(400, 'role must be 0, 1 or 2');
			p.role = r;
			return json({ ok: true, role: r });
		}],
		['GET', /^\/api\/players\/([0-9a-f]{32})\/steam$/, function (m) { return json(steamView(byGuid(m[1]))); }],
		['POST', /^\/api\/players\/([0-9a-f]{32})\/steam\/search$/, function (m) {
			var p = byGuid(m[1]);
			if (p.steamId) return fail(409, "this player's SteamID is already known");
			searches[p.guid] = { startedAt: Date.now() };
			return json(steamView(p));
		}],
		['POST', /^\/api\/players\/(\d+)\/(message|kick|ban)$/, function (m, o) {
			var b = body(o), p = online().filter(function (x) { return x.id === +m[1]; })[0];
			if (m[2] === 'message') {
				if (!String(b.message || '').trim()) return fail(400, 'message is required');
				return json({ ok: true, acked: true });
			}
			if (!p) return fail(404, 'player is no longer connected');
			var reason = String(b.reason || '').trim() || 'No reason specified';
			if (m[2] === 'kick') { kick(p, reason); return json({ ok: true, acked: true }); }
			if (b.minutes < 0) return fail(400, 'minutes must be 0 or greater');
			return ban(p, reason, b.minutes || 0);
		}],

		['GET', /^\/api\/console$/, function (m, o, url) {
			var after = parseInt(new URL(url, location.href).searchParams.get('after'), 10) || 0;
			return json({ ok: true, events: events.filter(function (e) { return e.seq > after; }), last: seq, connected: true });
		}],
		['POST', /^\/api\/console$/, function (m, o) {
			var cmd = String(body(o).command || '').trim();
			if (!cmd) return fail(400, 'no command');
			var now = new Date().toISOString();
			events.push({ seq: ++seq, time: now, kind: 'sent', text: '> ' + cmd, playerId: -1 });
			var out = consoleReply(cmd);
			if (out) events.push({ seq: ++seq, time: now, kind: 'reply', text: out, playerId: -1 });
			events.push({ seq: ++seq, time: now, kind: 'other', text: '(preview: nothing was sent to a server)', playerId: -1 });
			return json({ ok: true, acked: true, output: out });
		}],

		['GET', /^\/api\/bans$/, function () {
			var list = bans.map(function (b, i) {
				var p = byGuid(b.guid);
				return { index: i, guid: b.guid, name: p ? p.name : (b.name || 'Unknown'), minutes: b.minutes, perm: b.perm, reason: b.reason };
			});
			return json({ ok: true, connected: true, bans: list, count: list.length });
		}],
		['POST', /^\/api\/bans\/([0-9a-f]{32})\/ban$/, function (m, o) {
			var b = body(o), p = byGuid(m[1]);
			if (!p) return fail(404, 'unknown player (this GUID has never been seen on the server)');
			if (b.minutes < 0) return fail(400, 'minutes must be 0 or greater');
			return ban(p, String(b.reason || '').trim(), b.minutes || 0);
		}],
		['GET', /^\/api\/bans\/export$/, function (m, o, url) {
			var q = new URL(url, location.href).searchParams.get('guids');
			var want = q ? q.split(',') : null;
			return json({ bans: bans.filter(function (b) { return b.perm && (!want || want.indexOf(b.guid) >= 0); }).map(function (b) {
				var p = byGuid(b.guid), e = { guid: b.guid, name: p ? p.name : (b.name || 'Unknown') };
				if (p && p.ip) e.ip = p.ip;
				e.reason = b.reason;
				if (p && p.steamId) e.steamId = p.steamId;
				return e;
			}) });
		}],
		['POST', /^\/api\/bans\/import$/, function (m, o) {
			var list = body(o).bans || [], res = { ok: true, added: 0, skipped: [], onServer: true, kicked: 0 };
			if (!list.length) return fail(400, 'no bans in file');
			list.forEach(function (e) {
				var g = String(e.guid || '').trim().toLowerCase(), name = String(e.name || '').trim() || 'Unknown';
				var label = name + ' (' + g.slice(0, 8) + ')';
				if (!/^[0-9a-f]{32}$/.test(g)) { res.skipped.push(label + ': not a BattlEye GUID'); return; }
				if (bans.some(function (b) { return b.guid === g && b.perm; })) { res.skipped.push(label + ': already banned permanently'); return; }
				var reason = String(e.reason || '').trim() || 'Imported ban';
				bans = bans.filter(function (b) { return b.guid !== g; });
				bans.push({ guid: g, name: name, perm: true, minutes: 'perm', reason: reason });
				res.added++;
				var p = byGuid(g);
				if (p && p.isOnline) { kick(p, reason); res.kicked++; }
			});
			return json(res);
		}],
		['POST', /^\/api\/bans\/([0-9a-f]{32})\/unban$/, function (m) {
			var n = bans.length;
			bans = bans.filter(function (b) { return b.guid !== m[1]; });
			return json({ ok: true, be_removed: bans.length < n, db_removed: true, removed: n - bans.length, saved: true });
		}],

		['GET', /^\/api\/flows$/, function () {
			var list = Object.keys(runs).map(function (k) { return runs[k]; }).filter(function (r) { return flows.some(function (f) { return f.id === r.flowId; }); });
			return json({ ok: true, flows: flowList(), runs: list });
		}],
		['POST', /^\/api\/flows$/, function (m, o) {
			var f = body(o), name = String(f.name || '').trim();
			if (!name) return fail(400, 'name is required');
			if (flows.some(function (x) { return x.id !== f.id && x.name.toLowerCase() === name.toLowerCase(); })) return fail(400, 'a flow named "' + name + '" already exists');
			if (f.trigger === 'schedule' && !parseCron(f.schedule)) return fail(400, 'schedule: not a valid cron expression');
			if (isNaN(secondsOf(f.delay))) return fail(400, 'delay: use seconds, or h m s such as 1m20s');
			var old = flows.filter(function (x) { return x.id === f.id; })[0];
			var rec = { id: old ? old.id : nextFlowId++, name: name, trigger: old ? old.trigger : f.trigger, schedule: f.schedule || '',
				delay: f.delay || '', steps: f.steps || [], onStop: f.onStop || [], scheduleEnabled: old ? old.scheduleEnabled : false };
			if (old) flows[flows.indexOf(old)] = rec; else flows.push(rec);
			return json({ ok: true, id: rec.id });
		}],
		['POST', /^\/api\/flows\/(\d+)\/(run|stop|enable|disable|delete)$/, function (m) {
			var f = flows.filter(function (x) { return x.id === +m[1]; })[0];
			if (!f) return fail(404, 'flow not found');
			var r = runs[f.id];
			switch (m[2]) {
				case 'run':
					if (f.trigger === 'join' || f.trigger === 'chat') return fail(400, 'this flow runs for a player, so only a player can start it');
					if (r && r.active) return fail(409, 'flow is already running');
					startRun(f, 'manual run');
					break;
				case 'stop':
					if (!r || !r.active) return fail(409, 'flow is not running');
					r.active = false; r.status = 'stopped';
					if (f.onStop.length) r.log.push(clock(Date.now() / 1000) + '  sending the When stopped steps');
					r.log.push(clock(Date.now() / 1000) + '  stopped');
					break;
				case 'enable': f.scheduleEnabled = true; break;
				case 'disable': f.scheduleEnabled = false; break;
				case 'delete':
					if (r && r.active) return fail(409, 'stop the flow before deleting it');
					flows.splice(flows.indexOf(f), 1); delete runs[f.id];
			}
			return json({ ok: true });
		}],
		['POST', /^\/api\/flows\/preview$/, function (m, o) {
			var f = body(o), delay = secondsOf(f.delay);
			if (isNaN(delay)) return json({ ok: false, error: 'delay: use seconds, or h m s such as 1m20s' });
			if (f.trigger !== 'schedule') return json({ ok: true });
			var c = parseCron(f.schedule);
			if (!c) return json({ ok: false, error: 'not a valid cron expression (5 fields: minute hour day-of-month month day-of-week)' });
			var next = [], t = Date.now() / 1000;
			for (var i = 0; i < 3; i++) { var n = cronNext(c, t); if (!n) break; next.push(fmtWhen(n + delay)); t = n; }
			var desc = GEN.crons[String(f.schedule).trim().replace(/\s+/g, ' ')] || 'Next runs (the preview can only describe the example schedules)';
			return json({ ok: true, description: desc, next: next });
		}],
		['POST', /^\/api\/flows\/reorder$/, function (m, o) {
			var ids = body(o).ids || [];
			flows.sort(function (a, b) { return ids.indexOf(a.id) - ids.indexOf(b.id); });
			return json({ ok: true });
		}],
		['GET', /^\/api\/flows\/export$/, function (m, o, url) {
			var q = new URL(url, location.href).searchParams.get('ids');
			var want = q ? q.split(',').map(Number) : null;
			return json({ flows: flows.filter(function (f) { return !want || want.indexOf(f.id) >= 0; }).map(function (f) {
				var e = { name: f.name, trigger: f.trigger };
				if (f.schedule) e.schedule = f.schedule;
				if (f.delay) e.delay = f.delay;
				e.steps = f.steps;
				if (f.onStop.length) e.onStop = f.onStop;
				return e;
			}) });
		}],
		['POST', /^\/api\/flows\/import$/, function (m, o) {
			var b = body(o), added = 0;
			if (!b.flows || !b.flows.length) return fail(400, 'no flows in file');
			b.flows.forEach(function (e) {
				var base = String(e.name || '').trim(), name = base, n = 2;
				while (flows.some(function (x) { return x.name.toLowerCase() === name.toLowerCase(); })) name = base + ' (' + n++ + ')';
				flows.push({ id: nextFlowId++, name: name, trigger: e.trigger || 'manual', schedule: e.schedule || '', delay: e.delay || '',
					steps: e.steps || [], onStop: e.onStop || [], scheduleEnabled: false });
				added++;
			});
			return json({ ok: true, added: added, skipped: [] });
		}],
		['POST', /^\/api\/flows\/command$/, function (m, o) {
			var c = String(body(o).command || '').toLowerCase().replace(/^#/, '');
			if (c === 'lock' || c === 'unlock') return json({ ok: true, message: '#' + c + ' sent (preview)' });
			if (c === 'shutdown') return json({ ok: true, acked: true, message: '#shutdown sent (preview)' });
			if (c === 'kickall') return json({ ok: true, message: 'Kicked ' + online().length + ' player(s) (preview: nobody was kicked)' });
			return fail(400, 'unknown command');
		}],

		['GET', /^\/api\/chat-commands$/, function () {
			return json({ ok: true, commands: GEN.commands, settings: chatSaved, defaults: GEN.defaults });
		}],
		['POST', /^\/api\/chat-commands$/, function (m, o) {
			chatSaved = body(o).settings || chatSaved;
			return json({ ok: true, settings: chatSaved });
		}]
	];

	var realFetch = window.fetch ? window.fetch.bind(window) : null;
	window.fetch = function (input, opt) {
		var url = typeof input === 'string' ? input : input.url;
		var method = ((opt && opt.method) || 'GET').toUpperCase();
		var path = new URL(url, location.href).pathname;
		var at = path.indexOf('/api/');
		if (at < 0 && !/\/health$/.test(path)) return realFetch ? realFetch(input, opt) : Promise.reject(new Error('offline'));
		path = at >= 0 ? path.slice(at) : '/health';
		for (var i = 0; i < routes.length; i++) {
			var r = routes[i];
			var m = r[0] === method && r[1].exec(path);
			if (m) {
				try { return r[2](m, opt, url); } catch (e) { return fail(500, String(e)); }
			}
		}
		return Promise.resolve(new Response('404 page not found', { status: 404 }));
	};
	navigator.sendBeacon = function () { return true; };

	// Pages call these in ways that would leave the preview.
	window.addEventListener('DOMContentLoaded', function () {
		// Exports would leave the page: build the file here instead.
		if (typeof window.downloadFile === 'function') {
			window.downloadFile = function (url) {
				var name = /\/api\/bans\//.test(url) ? 'bans.json' : 'flows.json';
				window.fetch(url).then(function (r) { return r.json(); }).then(function (out) {
					var a = document.createElement('a');
					a.href = URL.createObjectURL(new Blob([JSON.stringify(out, null, 2) + '\n'], { type: 'application/json' }));
					a.download = name;
					document.body.appendChild(a); a.click(); a.remove();
				});
			};
		}
		var css = document.createElement('style');
		css.textContent = '.preview-banner { margin: 0 0 14px; padding: 8px 12px; border: 1px solid #5c4a12; border-radius: 6px; background: #2a2410; color: #fdd663; font-size: 13px; }';
		document.head.appendChild(css);
		var bar = document.createElement('div');
		bar.className = 'preview-banner';
		bar.innerHTML = '<b>Preview</b> with sample data. Buttons and dialogs work, but nothing is sent to a game server, and reloading starts over.';
		document.body.insertBefore(bar, document.body.firstChild);
	});
})();
