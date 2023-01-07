package conn

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/big"
	"math/rand"
	"net"
	"time"

	"msh/lib/config"
	"msh/lib/errco"
	"msh/lib/progmgr"
	"msh/lib/utility"
)

// reference:
// - wiki.vg/Query
// - github.com/dreamscached/minequery/v2

// clib is a group of query challenges
var clib *challengeLibrary = &challengeLibrary{}

// challenge represents a query challenge uint32 value and its expiration timer
type challenge struct {
	time.Timer
	val uint32
}

// challengeLibrary represents a group of query challenges
type challengeLibrary struct {
	list []challenge
}

// HandlerQuery handles query stats requests.
//
// Accepts requests on config.MshHost, config.MshPortQuery
func HandlerQuery() {
	// TODO
	// respond with real server info
	// emulate/forward depending on server status

	connCli, err := net.ListenPacket("udp", fmt.Sprintf("%s:%d", config.MshHost, config.MshPortQuery))
	if err != nil {
		errco.NewLogln(errco.TYPE_ERR, errco.LVL_3, errco.ERROR_CLIENT_LISTEN, err.Error())
		return
	}

	// infinite cycle to handle new clients queries
	errco.NewLogln(errco.TYPE_INF, errco.LVL_3, errco.ERROR_NIL, "listening for new clients queries\ton %s:%d ...", config.MshHost, config.MshPortQuery)
	for {
		// handshake / stats request read
		var buf []byte = make([]byte, 1024)
		n, addrCli, err := connCli.ReadFrom(buf)
		if err != nil {
			errco.NewLogln(errco.TYPE_ERR, errco.LVL_3, errco.ERROR_CONN_READ, err.Error())
			continue
		}

		// if minecraft server is not warm, handle request
		logMsh := handleRequest(connCli, addrCli, buf[:n])
		if logMsh != nil {
			logMsh.Log(true)
		}
	}
}

// handleRequest handles handshake / stats request from client performing handshake / stats response.
func handleRequest(connCli net.PacketConn, addr net.Addr, req []byte) *errco.MshLog {
	switch len(req) {

	case 7: // handshake request from client
		errco.NewLogln(errco.TYPE_BYT, errco.LVL_4, errco.ERROR_NIL, "recv handshake request:\t%v", req)

		sessionID := req[3:7]

		// handshake response composition
		res := bytes.NewBuffer([]byte{9})                       // type: handshake
		res.Write(sessionID)                                    // session id
		res.WriteString(fmt.Sprintf("%d", clib.gen()) + "\x00") // challenge (int32 written as string, null terminated)

		// handshake response send
		errco.NewLogln(errco.TYPE_BYT, errco.LVL_4, errco.ERROR_NIL, "send handshake response:\t%v", res.Bytes())
		_, err := connCli.WriteTo(res.Bytes(), addr)
		if err != nil {
			return errco.NewLog(errco.TYPE_ERR, errco.LVL_3, errco.ERROR_CONN_WRITE, err.Error())
		}

		return nil

	case 11, 15: // full / base stats request from client
		errco.NewLogln(errco.TYPE_BYT, errco.LVL_4, errco.ERROR_NIL, "recv stats request:\t%v", req)

		sessionID := req[3:7]
		challenge := req[7:11]

		// check that received challenge is known and not expired
		if !clib.inLibrary(binary.BigEndian.Uint32(challenge)) {
			return errco.NewLog(errco.TYPE_WAR, errco.LVL_3, errco.ERROR_QUERY_CHALLENGE, "challenge failed")
		}

		switch len(req) {
		case 11: // base stats response
			statRespBase(connCli, addr, sessionID)
		case 15: // full stats response
			statRespFull(connCli, addr, sessionID)
		}

		return nil

	default:
		return errco.NewLog(errco.TYPE_ERR, errco.LVL_3, errco.ERROR_CONN_READ, "unexpected number of bytes in stats / handshake request")
	}
}

// statRespBase writes a base stats response to udp connection
func statRespBase(connCli net.PacketConn, addr net.Addr, sessionID []byte) {
	var buf bytes.Buffer
	buf.WriteByte(0)                                                                 // type
	buf.Write(sessionID)                                                             // session ID
	buf.WriteString(fmt.Sprintf("%s\x00", config.ConfigRuntime.Msh.InfoHibernation)) // MOTD
	buf.WriteString("SMP\x00")                                                       // gametype hardcoded (default)
	levelName, _ := config.ConfigRuntime.ParsePropertiesString("level-name")
	buf.WriteString(fmt.Sprintf("%s\x00", levelName))                                      // map
	buf.WriteString("0\x00")                                                               // numplayers hardcoded
	buf.WriteString("0\x00")                                                               // maxplayers hardcoded
	buf.Write(append(utility.Reverse(big.NewInt(int64(config.MshPort)).Bytes()), byte(0))) // hostport
	buf.WriteString(fmt.Sprintf("%s\x00", utility.GetOutboundIP4()))                       // hostip

	errco.NewLogln(errco.TYPE_BYT, errco.LVL_4, errco.ERROR_NIL, "send stats base response:\t%v", buf.Bytes())
	_, err := connCli.WriteTo(buf.Bytes(), addr)
	if err != nil {
		errco.NewLogln(errco.TYPE_ERR, errco.LVL_3, errco.ERROR_CONN_WRITE, err.Error())
	}
}

// statRespFull writes a full stats response to udp connection
func statRespFull(connCli net.PacketConn, addr net.Addr, sessionID []byte) {
	var buf bytes.Buffer
	buf.WriteByte(0)                        // type
	buf.Write(sessionID)                    // session ID
	buf.WriteString("splitnum\x00\x80\x00") // padding (default)

	// K, V section
	buf.WriteString(fmt.Sprintf("hostname\x00%s\x00", config.ConfigRuntime.Msh.InfoHibernation))
	buf.WriteString(fmt.Sprintf("gametype\x00%s\x00", "SMP"))      // hardcoded (default)
	buf.WriteString(fmt.Sprintf("game_id\x00%s\x00", "MINECRAFT")) // hardcoded (default)
	buf.WriteString(fmt.Sprintf("version\x00%s\x00", config.ConfigRuntime.Server.Version))
	buf.WriteString(fmt.Sprintf("plugins\x00msh/%s: msh %s\x00", config.ConfigRuntime.Server.Version, progmgr.MshVersion)) // example: "plugins\x00{ServerVersion}: {Name} {Version}; {Name} {Version}\x00"
	levelName, _ := config.ConfigRuntime.ParsePropertiesString("level-name")
	buf.WriteString(fmt.Sprintf("map\x00%s\x00", levelName))
	buf.WriteString("numplayers\x000\x00") // hardcoded
	buf.WriteString("maxplayers\x000\x00") // hardcoded
	buf.WriteString(fmt.Sprintf("hostport\x00%d\x00", config.MshPort))
	buf.WriteString(fmt.Sprintf("hostip\x00%s\x00", utility.GetOutboundIP4()))
	buf.WriteByte(0) // termination of section (?)

	// Players
	buf.WriteString("\x01player_\x00\x00") // padding (default)
	buf.WriteString("\x00")                // example: "aaa\x00bbb\x00\x00"

	errco.NewLogln(errco.TYPE_BYT, errco.LVL_4, errco.ERROR_NIL, "send stats full response:\t%v", buf.Bytes())
	_, err := connCli.WriteTo(buf.Bytes(), addr)
	if err != nil {
		errco.NewLogln(errco.TYPE_ERR, errco.LVL_3, errco.ERROR_CONN_WRITE, err.Error())
	}
}

// Gen generates a int32 challenge and adds it to the challenge library
func (cl *challengeLibrary) gen() uint32 {
	rand.Seed(time.Now().UnixNano())
	cval := uint32(rand.Int31n(9_999_999-1_000_000+1) + 1_000_000)

	c := challenge{
		Timer: *time.NewTimer(time.Hour),
		val:   cval,
	}

	cl.list = append(cl.list, c)

	return cval
}

// InLibrary searches library for non-expired test value
func (cl *challengeLibrary) inLibrary(t uint32) bool {
	// remove expired challenges
	// (reverse list loop to remove elements while iterating on them)
	for i := len(cl.list) - 1; i >= 0; i-- {
		select {
		case <-cl.list[i].C:
			cl.list = append(cl.list[:i], cl.list[i+1:]...)
		default:
		}
	}

	// search for non-expired test value
	for i := 0; i < len(cl.list); i++ {
		if t == cl.list[i].val {
			return true
		}
	}

	return false
}
