// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

/// Plain ERC-20 returning bool.
contract Token {
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;
    mapping(address => bool) public blocked; // USDC-style blacklist
    uint256 public feeBps;                   // fee-on-transfer when > 0
    bool public noReturn;                    // USDT-style: return nothing
    function mint(address to, uint256 v) external { balanceOf[to] += v; }
    function approve(address s, uint256 v) external returns (bool) { allowance[msg.sender][s] = v; return true; }
    function setBlocked(address a, bool b) external { blocked[a] = b; }
    function setFee(uint256 b) external { feeBps = b; }
    function setNoReturn(bool b) external { noReturn = b; }
    function _move(address f, address t, uint256 v) internal {
        require(!blocked[f] && !blocked[t], "blocked");
        balanceOf[f] -= v;
        uint256 fee = v * feeBps / 10000;
        balanceOf[t] += v - fee;
    }
    function transfer(address t, uint256 v) external virtual returns (bool) {
        _move(msg.sender, t, v);
        if (noReturn) assembly { return(0, 0) }
        return true;
    }
    function transferFrom(address f, address t, uint256 v) external returns (bool) {
        allowance[f][msg.sender] -= v;
        _move(f, t, v);
        if (noReturn) assembly { return(0, 0) }
        return true;
    }
}

/// Token that returns false instead of reverting.
contract FalseToken is Token {
    bool public failTransfers;
    function setFail(bool b) external { failTransfers = b; }
    function transfer(address t, uint256 v) external override returns (bool) { if (failTransfers) return false; _move(msg.sender, t, v); return true; }
}

/// Token that tries to re-enter the swap contract during transfer.
contract ReentrantToken is Token {
    address public target;
    bytes public payload;
    bool public reentered;
    bool public reentrySucceeded;
    function arm(address t, bytes calldata p) external { target = t; payload = p; }
    function transfer(address t, uint256 v) external override returns (bool) {
        if (target != address(0) && !reentered) {
            reentered = true;
            (bool ok,) = target.call(payload);
            reentrySucceeded = ok;
        }
        _move(msg.sender, t, v);
        return true;
    }
}

/// Returns a huge return-data blob (return-data bomb) on transfer.
contract BombToken is Token {
    function transfer(address t, uint256 v) external override returns (bool) {
        _move(msg.sender, t, v);
        assembly { return(0, 100000) }
    }
}

contract RejectEth { receive() external payable { revert("no"); } }

/// USDT-style token: transfer / transferFrom / approve are declared with NO return
/// value at the ABI level (like Tether on Ethereum mainnet), not just an early return.
contract UsdtLike {
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;
    function mint(address to, uint256 v) external { balanceOf[to] += v; }
    function approve(address s, uint256 v) external { allowance[msg.sender][s] = v; }
    function transfer(address t, uint256 v) external { balanceOf[msg.sender] -= v; balanceOf[t] += v; }
    function transferFrom(address f, address t, uint256 v) external {
        allowance[f][msg.sender] -= v;
        balanceOf[f] -= v;
        balanceOf[t] += v;
    }
}
