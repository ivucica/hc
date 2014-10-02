package handler

import(
    "github.com/brutella/hap/db"
    "github.com/brutella/hap/netio/pair"
    "github.com/brutella/hap/netio"
        
    "io/ioutil"
    "net/http"
    "fmt"
)

type PairSetupHandler struct {
    http.Handler
    
    bridge *netio.Bridge
    database *db.Manager
    context netio.Context
}

func NewPairSetupHandler(bridge *netio.Bridge, database *db.Manager, context netio.Context) *PairSetupHandler {
    handler := PairSetupHandler{
                bridge: bridge,
                database: database,
                context: context,
            }
    
    return &handler
}

func (handler *PairSetupHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
    fmt.Println("POST /pair-setup")
    response.Header().Set("Content-Type", netio.HTTPContentTypePairingTLV8)
    
    key := handler.context.GetConnectionKey(request)
    session := handler.context.Get(key).(netio.Session)
    controller := session.PairSetupHandler()
    if controller == nil {
        fmt.Println("Create new pair setup controller")
        var err error
        controller, err = pair.NewSetupServerController(handler.bridge, handler.database)
        if err != nil {
            fmt.Println(err)
        }
        
        session.SetPairSetupHandler(controller)
    }
    
    res, err := pair.HandleReaderForHandler(request.Body, controller)
    
    if err != nil {
        fmt.Println(err)
        response.WriteHeader(http.StatusInternalServerError)
    } else {
        bytes, _ := ioutil.ReadAll(res)
        response.Write(bytes)
    }
}