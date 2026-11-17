// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

/// Mock USDC for public-testnet trials only. 6 decimals, mintable by the
/// deployer. This is not the real USD Coin and is not for mainnet.
contract MockUSDC {
    string public constant name = "Mock USDC (test only)";
    string public constant symbol = "USDC";
    uint8 public constant decimals = 6;

    uint256 public totalSupply;
    address public minter;
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    event Transfer(address indexed from, address indexed to, uint256 value);
    event Approval(address indexed owner, address indexed spender, uint256 value);

    constructor() {
        minter = msg.sender;
    }

    function mint(address to, uint256 v) external {
        require(msg.sender == minter, "not minter");
        require(to != address(0) && v > 0, "bad mint");
        totalSupply += v;
        balanceOf[to] += v;
        emit Transfer(address(0), to, v);
    }

    function approve(address s, uint256 v) external returns (bool) {
        allowance[msg.sender][s] = v;
        emit Approval(msg.sender, s, v);
        return true;
    }

    function transfer(address t, uint256 v) external returns (bool) {
        _move(msg.sender, t, v);
        return true;
    }

    function transferFrom(address f, address t, uint256 v) external returns (bool) {
        uint256 a = allowance[f][msg.sender];
        require(a >= v, "allowance");
        if (a != type(uint256).max) allowance[f][msg.sender] = a - v;
        _move(f, t, v);
        return true;
    }

    function _move(address f, address t, uint256 v) internal {
        require(t != address(0), "zero");
        require(balanceOf[f] >= v, "balance");
        balanceOf[f] -= v;
        balanceOf[t] += v;
        emit Transfer(f, t, v);
    }
}
