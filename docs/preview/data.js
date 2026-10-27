// Written by TestPreview (RECON_PREVIEW=1 go test -run TestPreview ./internal/web). Don't edit.
window.RECON_PREVIEW_DATA = {
	"commands": [
		{
			"cmd": "help",
			"staff": false,
			"cool": "0",
			"announce": false,
			"length": false,
			"warn": false,
			"help": "!help lists the commands. !help !\u003ccommand\u003e explains one.",
			"fields": [
				{
					"key": "list",
					"label": "!help reply (to the player)",
					"def": "Commands: {commands}. Type !help !\u003ccommand\u003e for details.",
					"ph": [
						"commands"
					]
				}
			]
		},
		{
			"cmd": "shout",
			"staff": false,
			"cool": "10s",
			"announce": false,
			"length": false,
			"warn": false,
			"help": "!shout \u003cmessage\u003e sends your message to everyone on the server.",
			"fields": [
				{
					"key": "msg",
					"label": "Message (to everyone)",
					"def": "({player_name}): {message}",
					"ph": [
						"player_name",
						"message"
					]
				}
			]
		},
		{
			"cmd": "whisper",
			"staff": false,
			"cool": "0",
			"announce": false,
			"length": false,
			"warn": false,
			"help": "!whisper \u003cplayer number\u003e \u003cmessage\u003e sends a private message to that player.",
			"fields": [
				{
					"key": "msg",
					"label": "Message (to the player number they typed)",
					"def": "({player_name}): {message}",
					"ph": [
						"player_name",
						"message"
					]
				}
			]
		},
		{
			"cmd": "players",
			"staff": false,
			"cool": "10s",
			"announce": false,
			"length": false,
			"warn": false,
			"help": "!players lists everyone online with their player number, for !whisper.",
			"fields": [
				{
					"key": "entry",
					"label": "Each player in the list (5 per line, a line every 3 seconds, to the player)",
					"def": "{target_name}:{target_id}",
					"ph": [
						"target_name",
						"target_id"
					]
				}
			]
		},
		{
			"cmd": "vote",
			"staff": false,
			"cool": "10m",
			"announce": false,
			"length": true,
			"warn": false,
			"help": "!vote \u003cquestion\u003e starts a server-wide vote on the topic you choose. Everyone answers with !vote y or !vote n.",
			"fields": [
				{
					"key": "start",
					"label": "Vote question (to everyone)",
					"def": "({player_name}) {topic}? Yes or No? Type !vote y or !vote n",
					"ph": [
						"player_name",
						"topic"
					]
				},
				{
					"key": "result",
					"label": "Result (to everyone, when the vote ends)",
					"def": "The server has spoken and the result is {vote_result}!",
					"ph": [
						"vote_result",
						"yes",
						"no",
						"topic"
					]
				}
			]
		},
		{
			"cmd": "code",
			"staff": false,
			"cool": "0",
			"announce": false,
			"length": false,
			"warn": false,
			"help": "!code sends your player code to the admins and mods online, so they can find you.",
			"fields": [
				{
					"key": "staff",
					"label": "Message (to each mod and admin online)",
					"def": "Code for {player_name} (#{player_id}): {code}",
					"ph": [
						"player_name",
						"player_id",
						"code"
					]
				}
			]
		},
		{
			"cmd": "kick",
			"staff": true,
			"cool": "0",
			"announce": true,
			"length": false,
			"warn": false,
			"help": "!kick \u003ccode or player number\u003e \u003creason\u003e kicks that player. Their code comes from !code, their number from !players.",
			"fields": [
				{
					"key": "staff",
					"label": "Notice (to each mod and admin online, always)",
					"def": "{target_name} has been kicked by {player_name}: {reason}",
					"ph": [
						"target_name",
						"player_name",
						"reason"
					]
				},
				{
					"key": "announce",
					"label": "Announcement (to everyone, when Announce is on)",
					"def": "{target_name} has been kicked",
					"ph": [
						"target_name",
						"reason"
					]
				}
			]
		},
		{
			"cmd": "ban",
			"staff": true,
			"cool": "0",
			"announce": true,
			"length": false,
			"warn": false,
			"help": "!ban \u003ccode or player number\u003e \u003creason\u003e permanently bans that player. Their code (the last 6 characters of their GUID) comes from !code, their number from !players.",
			"fields": [
				{
					"key": "staff",
					"label": "Notice (to each mod and admin online, always)",
					"def": "{target_name} has been banned by {player_name}: {reason}",
					"ph": [
						"target_name",
						"player_name",
						"reason"
					]
				},
				{
					"key": "announce",
					"label": "Announcement (to everyone, when Announce is on)",
					"def": "{target_name} has been banned",
					"ph": [
						"target_name",
						"reason"
					]
				}
			]
		},
		{
			"cmd": "warning",
			"staff": true,
			"cool": "0",
			"announce": true,
			"length": false,
			"warn": true,
			"help": "!warning \u003ccode or player number\u003e \u003creason\u003e warns that player. Enough warnings in one visit and they are kicked or banned.",
			"fields": [
				{
					"key": "player",
					"label": "Warning (to the player)",
					"def": "Warning {warnings}/{warn_count} from {player_name}: {reason}",
					"ph": [
						"target_name",
						"player_name",
						"reason",
						"warnings",
						"warn_count"
					]
				},
				{
					"key": "staff",
					"label": "Notice (to each mod and admin online, always)",
					"def": "{target_name} was warned by {player_name} ({warnings}/{warn_count}): {reason}",
					"ph": [
						"target_name",
						"player_name",
						"reason",
						"warnings",
						"warn_count"
					]
				},
				{
					"key": "announce",
					"label": "Announcement (to everyone, when Announce is on)",
					"def": "{target_name} has been warned: {reason}",
					"ph": [
						"target_name",
						"reason",
						"warnings",
						"warn_count"
					]
				},
				{
					"key": "limit_staff",
					"label": "At the threshold: notice (to each mod and admin online, always)",
					"def": "{target_name} reached {warn_count} warnings and has been {action}: {reason}",
					"ph": [
						"target_name",
						"player_name",
						"reason",
						"warn_count",
						"action"
					]
				},
				{
					"key": "limit_announce",
					"label": "At the threshold: announcement (to everyone, when Announce is on)",
					"def": "{target_name} has been {action}",
					"ph": [
						"target_name",
						"reason",
						"action"
					]
				}
			]
		}
	],
	"crons": {
		"*/20 * * * *": "Every 20 minutes",
		"0 */6 * * *": "At 00:00, 06:00, 12:00 and 18:00, every day",
		"0 0,6,12,18 * * *": "At 00:00, 06:00, 12:00 and 18:00, every day",
		"0 5 * * 1-5": "At 05:00, on weekdays",
		"0 5/6 * * *": "At 05:00, 11:00, 17:00 and 23:00, every day",
		"0 6 * * *": "At 06:00, every day",
		"30 4 * * 1,4": "At 04:30, on Monday and Thursday",
		"55 23,5,11,17 * * *": "At 05:55, 11:55, 17:55 and 23:55, every day"
	},
	"defaults": {
		"ban": {
			"on": true,
			"cool": "0",
			"announce": true,
			"msgs": {
				"announce": "{target_name} has been banned",
				"staff": "{target_name} has been banned by {player_name}: {reason}"
			}
		},
		"code": {
			"on": true,
			"cool": "0",
			"msgs": {
				"staff": "Code for {player_name} (#{player_id}): {code}"
			}
		},
		"help": {
			"on": true,
			"cool": "0",
			"msgs": {
				"list": "Commands: {commands}. Type !help !\u003ccommand\u003e for details."
			}
		},
		"kick": {
			"on": true,
			"cool": "0",
			"announce": true,
			"msgs": {
				"announce": "{target_name} has been kicked",
				"staff": "{target_name} has been kicked by {player_name}: {reason}"
			}
		},
		"players": {
			"on": true,
			"cool": "10s",
			"msgs": {
				"entry": "{target_name}:{target_id}"
			}
		},
		"shout": {
			"on": true,
			"cool": "10s",
			"msgs": {
				"msg": "({player_name}): {message}"
			}
		},
		"vote": {
			"on": true,
			"cool": "10m",
			"length": "5m",
			"msgs": {
				"result": "The server has spoken and the result is {vote_result}!",
				"start": "({player_name}) {topic}? Yes or No? Type !vote y or !vote n"
			}
		},
		"warning": {
			"on": true,
			"cool": "0",
			"announce": true,
			"threshold": 2,
			"action": "ban",
			"msgs": {
				"announce": "{target_name} has been warned: {reason}",
				"limit_announce": "{target_name} has been {action}",
				"limit_staff": "{target_name} reached {warn_count} warnings and has been {action}: {reason}",
				"player": "Warning {warnings}/{warn_count} from {player_name}: {reason}",
				"staff": "{target_name} was warned by {player_name} ({warnings}/{warn_count}): {reason}"
			}
		},
		"whisper": {
			"on": true,
			"cool": "0",
			"msgs": {
				"msg": "({player_name}): {message}"
			}
		}
	},
	"examples": {
		"flows": [
			{
				"name": "Welcome Player",
				"trigger": "join",
				"delay": "3s",
				"steps": [
					{
						"type": "if",
						"branches": [
							{
								"subject": "player_name",
								"op": "eq",
								"value": "Survivor",
								"steps": [
									{
										"type": "kick",
										"who": "player",
										"reason": "Name must not be Survivor"
									}
								]
							}
						],
						"else": [
							{
								"type": "message",
								"to": "player",
								"text": "Welcome {player_name} to our DayZ server!"
							}
						]
					}
				]
			},
			{
				"name": "MOTD",
				"trigger": "schedule",
				"schedule": "*/20 * * * *",
				"delay": "3s",
				"steps": [
					{
						"type": "message",
						"to": "all",
						"text": "Admins are active, please play respectfully."
					},
					{
						"type": "wait",
						"time": "2s"
					},
					{
						"type": "message",
						"to": "all",
						"text": "If you need admin assistance, send a message in game or type !help."
					}
				]
			},
			{
				"name": "Server Restart",
				"trigger": "schedule",
				"schedule": "0 5/6 * * *",
				"steps": [
					{
						"type": "message",
						"to": "all",
						"text": "Server is going to restart in 60 minutes."
					},
					{
						"type": "wait",
						"time": "30m"
					},
					{
						"type": "message",
						"to": "all",
						"text": "Server is going to restart in 30 minutes."
					},
					{
						"type": "wait",
						"time": "15m"
					},
					{
						"type": "message",
						"to": "all",
						"text": "Server is going to restart in 15 minutes."
					},
					{
						"type": "wait",
						"time": "5m"
					},
					{
						"type": "message",
						"to": "all",
						"text": "Server is going to restart in 10 minutes."
					},
					{
						"type": "wait",
						"time": "5m"
					},
					{
						"type": "message",
						"to": "all",
						"text": "Server is going to restart in 5 minutes."
					},
					{
						"type": "wait",
						"time": "2m"
					},
					{
						"type": "message",
						"to": "all",
						"text": "Server is going to restart in 3 minutes."
					},
					{
						"type": "wait",
						"time": "1m"
					},
					{
						"type": "lock"
					},
					{
						"type": "message",
						"to": "all",
						"text": "Server is going to restart in \u003c 2 minutes, you will be kicked."
					},
					{
						"type": "wait",
						"time": "1m50s"
					},
					{
						"type": "kickAll",
						"reason": "Server Restart"
					},
					{
						"type": "wait",
						"time": "10s"
					},
					{
						"type": "shutdown"
					}
				],
				"onStop": [
					{
						"type": "message",
						"to": "all",
						"text": "Server restart has been postponed."
					},
					{
						"type": "unlock"
					}
				]
			}
		]
	}
};
